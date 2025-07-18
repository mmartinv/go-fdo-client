// SPDX-FileCopyrightText: (C) 2025 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fido-device-onboard/go-fdo"
	"github.com/fido-device-onboard/go-fdo-client/internal/tls"
	"github.com/fido-device-onboard/go-fdo-client/internal/tpm_utils"
	"github.com/fido-device-onboard/go-fdo/cose"
	"github.com/fido-device-onboard/go-fdo/fsim"
	"github.com/fido-device-onboard/go-fdo/kex"
	"github.com/fido-device-onboard/go-fdo/protocol"
	"github.com/fido-device-onboard/go-fdo/serviceinfo"
	"github.com/spf13/cobra"
)

type fsVar map[string]string

var (
	cipherSuite string
	dlDir       string
	echoCmds    bool
	kexSuite    string
	rvOnly      bool
	resale      bool
	uploads     = make(fsVar)
	wgetDir     string
)
var validCipherSuites = []string{
	"A128GCM", "A192GCM", "A256GCM",
	"AES-CCM-64-128-128", "AES-CCM-64-128-256",
	"COSEAES128CBC", "COSEAES128CTR",
	"COSEAES256CBC", "COSEAES256CTR",
}
var validKexSuites = []string{
	"DHKEXid14", "DHKEXid15", "ASYMKEX2048", "ASYMKEX3072", "ECDH256", "ECDH384",
}

var onboardCmd = &cobra.Command{
	Use:   "onboard",
	Short: "Run FDO TO1 and TO2 onboarding",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateOnboardFlags(); err != nil {
			return fmt.Errorf("Validation error: %v", err)
		}
		if debug {
			level.Set(slog.LevelDebug)
		}

		if tpmPath != "" {
			var err error
			tpmc, err = tpm_utils.TpmOpen(tpmPath)
			if err != nil {
				return err
			}
			defer tpmc.Close()
		}

		deviceStatus, err := loadDeviceStatus()
		if err != nil {
			return fmt.Errorf("load device status failed: %w", err)
		}

		printDeviceStatus(deviceStatus)

		if deviceStatus == FDO_STATE_PRE_TO1 || (deviceStatus == FDO_STATE_IDLE && resale) {
			return doOnboard()
		} else if deviceStatus == FDO_STATE_IDLE {
			fmt.Println("FDO in Idle State. Device Onboarding already completed")
		} else if deviceStatus == FDO_STATE_PRE_DI {
			return fmt.Errorf("Device has not been properly initialized: run device-init first")
		} else {
			return fmt.Errorf("Device state is invalid: %v", deviceStatus)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(onboardCmd)
	onboardCmd.Flags().StringVar(&cipherSuite, "cipher", "A128GCM", "Name of cipher suite to use for encryption (see usage)")
	onboardCmd.Flags().StringVar(&dlDir, "download", "", "A dir to download files into (FSIM disabled if empty)")
	onboardCmd.Flags().StringVar(&diKey, "key", "", "Key type for device credential [options: ec256, ec384, rsa2048, rsa3072]")
	onboardCmd.Flags().BoolVar(&echoCmds, "echo-commands", false, "Echo all commands received to stdout (FSIM disabled if false)")
	onboardCmd.Flags().StringVar(&kexSuite, "kex", "", "Name of cipher suite to use for key exchange (see usage)")
	onboardCmd.Flags().BoolVar(&insecureTLS, "insecure-tls", false, "Skip TLS certificate verification")
	onboardCmd.Flags().BoolVar(&rvOnly, "rv-only", false, "Perform TO1 then stop")
	onboardCmd.Flags().BoolVar(&resale, "resale", false, "Perform resale")
	onboardCmd.Flags().Var(&uploads, "upload", "List of dirs and files to upload files from, comma-separated and/or flag provided multiple times (FSIM disabled if empty)")
	onboardCmd.Flags().StringVar(&wgetDir, "wget-dir", "", "A dir to wget files into (FSIM disabled if empty)")

	onboardCmd.MarkFlagRequired("key")
	onboardCmd.MarkFlagRequired("kex")
}

func doOnboard() error {
	// Read device credential blob to configure client for TO1/TO2
	dc, hmacSha256, hmacSha384, privateKey, cleanup, err := readCred()
	if err == nil && cleanup != nil {
		defer func() { _ = cleanup() }()
	}
	if err != nil {
		return err
	}

	// Try TO1+TO2
	kexCipherSuiteID, ok := kex.CipherSuiteByName(cipherSuite)
	if !ok {
		return fmt.Errorf("invalid key exchange cipher suite: %s", cipherSuite)
	}
	newDC := transferOwnership(clientContext, dc.RvInfo, fdo.TO2Config{
		Cred:       *dc,
		HmacSha256: hmacSha256,
		HmacSha384: hmacSha384,
		Key:        privateKey,
		Devmod: serviceinfo.Devmod{
			Os:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: "Debian Bookworm",
			Device:  "go-validation",
			FileSep: ";",
			Bin:     runtime.GOARCH,
		},
		KeyExchange:          kex.Suite(kexSuite),
		CipherSuite:          kexCipherSuiteID,
		AllowCredentialReuse: true,
	})
	if rvOnly {
		return nil
	}
	if newDC == nil {
		fmt.Println("Credential not updated (either due to failure of TO2 or the Credential Reuse Protocol")
		return nil
	}

	// Store new credential
	fmt.Println("FIDO Device Onboard Complete")
	return updateCred(*newDC, FDO_STATE_IDLE)
}

func transferOwnership(ctx context.Context, rvInfo [][]protocol.RvInstruction, conf fdo.TO2Config) *fdo.DeviceCredential { //nolint:gocyclo
	var to2URLs []string
	directives := protocol.ParseDeviceRvInfo(rvInfo)
	for _, directive := range directives {
		if !directive.Bypass {
			continue
		}
		for _, url := range directive.URLs {
			to2URLs = append(to2URLs, url.String())
		}
	}

	// Try TO1 on each address only once
	var to1d *cose.Sign1[protocol.To1d, []byte]
TO1:
	for _, directive := range directives {
		if directive.Bypass {
			continue
		}

		for _, url := range directive.URLs {
			var err error
			to1d, err = fdo.TO1(context.TODO(), tls.TlsTransport(url.String(), nil, insecureTLS), conf.Cred, conf.Key, nil)
			if err != nil {
				slog.Error("TO1 failed", "base URL", url.String(), "error", err)
				continue
			}
			break TO1
		}

		if directive.Delay != 0 {
			// A 25% plus or minus jitter is allowed by spec
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(directive.Delay):
			}
		}
	}
	if to1d != nil {
		for _, to2Addr := range to1d.Payload.Val.RV {
			if to2Addr.DNSAddress == nil && to2Addr.IPAddress == nil {
				slog.Error("Error: Both IP and DNS can't be null")
				continue
			}

			var scheme, port string
			switch to2Addr.TransportProtocol {
			case protocol.HTTPTransport:
				scheme, port = "http://", "80"
			case protocol.HTTPSTransport:
				scheme, port = "https://", "443"
			default:
				continue
			}
			if to2Addr.Port != 0 {
				port = strconv.Itoa(int(to2Addr.Port))
			}

			// Check and add DNS address if valid and resolvable
			if to2Addr.DNSAddress != nil && isResolvableDNS(*to2Addr.DNSAddress) {
				host := *to2Addr.DNSAddress
				to2URLs = append(to2URLs, scheme+net.JoinHostPort(host, port))
			}

			// Check and add IP address if valid
			if to2Addr.IPAddress != nil && isValidIP(to2Addr.IPAddress.String()) {
				host := to2Addr.IPAddress.String()
				to2URLs = append(to2URLs, scheme+net.JoinHostPort(host, port))
			}
		}
	}

	// Print TO2 addrs if RV-only
	if rvOnly {
		if to1d != nil {
			fmt.Printf("TO1 Blob: %+v\n", to1d.Payload.Val)
		}
		return nil
	}

	// Try TO2 on each address only once
	for _, baseURL := range to2URLs {
		newDC := transferOwnership2(tls.TlsTransport(baseURL, nil, insecureTLS), to1d, conf)
		if newDC != nil {
			return newDC
		}
	}

	return nil
}

func transferOwnership2(transport fdo.Transport, to1d *cose.Sign1[protocol.To1d, []byte], conf fdo.TO2Config) *fdo.DeviceCredential {
	fsims := map[string]serviceinfo.DeviceModule{
		"fido_alliance": &fsim.Interop{},
	}
	if dlDir != "" {
		fsims["fdo.download"] = &fsim.Download{
			CreateTemp: func() (*os.File, error) {
				tmpFile, err := os.CreateTemp(dlDir, ".fdo.download_*")
				if err != nil {
					return nil, err
				}
				return tmpFile, nil
			},
			NameToPath: func(name string) string {
				cleanName := filepath.Clean(name)
				if !filepath.IsAbs(cleanName) {
					return filepath.Join(dlDir, cleanName)
				}
				return filepath.Join(dlDir, filepath.Base(cleanName))
			},
		}
	}
	if echoCmds {
		fsims["fdo.command"] = &fsim.Command{
			Timeout: time.Second,
			Transform: func(cmd string, args []string) (string, []string) {
				sanitizedArgs := make([]string, len(args))
				for i, arg := range args {
					sanitizedArgs[i] = fmt.Sprintf("%q", arg)
				}
				return "sh", []string{"-c", fmt.Sprintf("echo %s", strings.Join(sanitizedArgs, " "))}
			},
		}
	}
	if len(uploads) > 0 {
		fsims["fdo.upload"] = &fsim.Upload{
			FS: uploads,
		}
	}
	if wgetDir != "" {
		fsims["fdo.wget"] = &fsim.Wget{
			CreateTemp: func() (*os.File, error) {
				tmpFile, err := os.CreateTemp(wgetDir, ".fdo.wget_*")
				if err != nil {
					return nil, err
				}
				return tmpFile, nil
			},
			NameToPath: func(name string) string {
				cleanName := filepath.Clean(name)
				if !filepath.IsAbs(cleanName) {
					return filepath.Join(wgetDir, cleanName)
				}
				return filepath.Join(wgetDir, filepath.Base(cleanName))
			},
			Timeout: 10 * time.Second,
		}
	}
	conf.DeviceModules = fsims

	cred, err := fdo.TO2(context.TODO(), transport, to1d, conf)
	if err != nil {
		slog.Error("TO2 failed", "error", err)
		return nil
	}
	return cred
}

// Function to validate if a string is a valid IP address
func isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

// Function to check if a DNS address is resolvable
func isResolvableDNS(dns string) bool {
	_, err := net.LookupHost(dns)
	return err == nil
}

func printDeviceStatus(status FdoDeviceState) {
	switch status {
	case FDO_STATE_PRE_DI:
		slog.Debug("Device is ready for DI")
	case FDO_STATE_PRE_TO1:
		slog.Debug("Device is ready for Ownership transfer")
	case FDO_STATE_IDLE:
		slog.Debug("Device Ownership transfer Done")
	case FDO_STATE_RESALE:
		slog.Debug("Device is ready for Ownership transfer")
	case FDO_STATE_ERROR:
		slog.Debug("Error in getting device status")
	}
}

func (files fsVar) String() string {
	if len(files) == 0 {
		return "[]"
	}
	paths := "["
	for path := range files {
		paths += path + ","
	}
	return paths[:len(paths)-1] + "]"
}

func (files fsVar) Set(paths string) error {
	for _, path := range strings.Split(paths, ",") {
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("[%q]: %w", path, err)
		}
		files[pathToName(path, abs)] = abs
	}
	return nil
}

func (files fsVar) Type() string {
	return "fsVar"
}

// Open implements fs.FS
func (files fsVar) Open(path string) (fs.File, error) {
	if !fs.ValidPath(path) {
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  fs.ErrInvalid,
		}
	}

	// TODO: Enforce chroot-like security
	if _, rootAccess := files["/"]; rootAccess {
		return os.Open(filepath.Clean(path))
	}

	name := pathToName(path, "")
	if abs, ok := files[name]; ok {
		return os.Open(filepath.Clean(abs))
	}
	for dir := filepath.Dir(name); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if abs, ok := files[dir]; ok {
			return os.Open(filepath.Clean(abs))
		}
	}
	return nil, &fs.PathError{
		Op:   "open",
		Path: path,
		Err:  fs.ErrNotExist,
	}
}

// The name of the directory or file is its cleaned path, if absolute. If the
// path given is relative, then remove all ".." and "." at the start. If the
// path given is only 1 or more ".." or ".", then use the name of the absolute
// path.
func pathToName(path, abs string) string {
	cleaned := filepath.Clean(path)
	if rooted := path[:1] == "/"; rooted {
		return cleaned
	}
	pathparts := strings.Split(cleaned, string(filepath.Separator))
	for len(pathparts) > 0 && (pathparts[0] == ".." || pathparts[0] == ".") {
		pathparts = pathparts[1:]
	}
	if len(pathparts) == 0 && abs != "" {
		pathparts = []string{filepath.Base(abs)}
	}
	return filepath.Join(pathparts...)
}

func validateOnboardFlags() error {
	if !slices.Contains(validCipherSuites, cipherSuite) {
		return fmt.Errorf("invalid cipher suite: %s", cipherSuite)
	}

	if dlDir != "" && (!isValidPath(dlDir) || !fileExists(dlDir)) {
		return fmt.Errorf("invalid download directory: %s", dlDir)
	}

	if err := validateDiKey(); err != nil {
		return err
	}

	if !slices.Contains(validKexSuites, kexSuite) {
		return fmt.Errorf("invalid key exchange suite: '%s', options [%s]",
			kexSuite, strings.Join(validKexSuites, ", "))
	}

	for path := range uploads {
		if !isValidPath(path) {
			return fmt.Errorf("invalid upload path: %s", path)
		}

		if !fileExists(path) {
			return fmt.Errorf("file doesn't exist: %s", path)
		}
	}

	if wgetDir != "" && (!isValidPath(wgetDir) || !fileExists(wgetDir)) {
		return fmt.Errorf("invalid wget directory: %s", wgetDir)
	}

	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}
