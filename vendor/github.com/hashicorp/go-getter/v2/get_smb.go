package getter

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/hashicorp/go-multierror"
)

// SmbGetter is a Getter implementation that will download a module from
// a shared folder using smbclient cli or looking for local mount.
type SmbGetter struct {
	getter
}

const basePathError = "samba path should contain valid host, filepath, and authentication if necessary (smb://<user>:<password>@<host>/<file_path>)"

func (g *SmbGetter) Mode(ctx context.Context, u *url.URL) (Mode, error) {
	if u.Host == "" || u.Path == "" {
		return 0, fmt.Errorf(basePathError)
	}

	// Look in a possible local mount of shared folder
	path := "/" + u.Host + u.Path
	if runtime.GOOS == "windows" {
		path = "/" + path
	}
	f := new(FileGetter)
	mode, result := f.mode(path)
	if result == nil {
		return mode, nil
	}

	// If not mounted, use smbclient cli to verify mode
	mode, err := g.smbClientMode(u)
	if err == nil {
		return mode, nil
	}

	result = multierror.Append(result, err)
	return 0, fmt.Errorf("one of the options should be available: \n 1. local mount of the smb shared folder or; \n 2. smbclient cli installed. \n err: %s", result.Error())
}

func (g *SmbGetter) smbClientMode(u *url.URL) (Mode, error) {
	hostPath, filePath, err := findHostAndFilePath(u)
	if err != nil {
		return 0, err
	}
	file := ""
	// Get file and subdirectory name when existent
	if strings.Contains(filePath, "/") {
		i := strings.LastIndex(filePath, "/")
		file = filePath[i+1:]
		filePath = filePath[:i]
	} else {
		file = filePath
		filePath = "."
	}

	baseCmd := smbclientBaseCmd(u.User, hostPath, filePath)
	// check if file exists in the smb shared folder and check the mode
	isDir, err := isDirectory(baseCmd, file)
	if err != nil {
		return 0, err
	}
	if isDir {
		return ModeDir, nil
	}
	return ModeFile, nil
}

func (g *SmbGetter) Get(ctx context.Context, req *Request) error {
	if req.u.Host == "" || req.u.Path == "" {
		return fmt.Errorf(basePathError)
	}

	// If dst folder doesn't exists, we need to remove the created on later in case of failures
	dstExisted := false
	if req.Dst != "" {
		if _, err := os.Lstat(req.Dst); err == nil {
			dstExisted = true
		}
	}

	// First look in a possible local mount of the shared folder
	path := "/" + req.u.Host + req.u.Path
	if runtime.GOOS == "windows" {
		path = "/" + path
	}
	f := new(FileGetter)
	result := f.get(path, req)
	if result == nil {
		return nil
	}

	// If not mounted, try downloading the directory content using smbclient cli
	err := g.smbclientGet(req)
	if err == nil {
		return nil
	}

	result = multierror.Append(result, err)

	if !dstExisted {
		// Remove the destination created for smbclient
		os.Remove(req.Dst)
	}

	return fmt.Errorf("one of the options should be available: \n 1. local mount of the smb shared folder or; \n 2. smbclient cli installed. \n err: %s", result.Error())
}

func (g *SmbGetter) smbclientGet(req *Request) error {
	hostPath, directory, err := findHostAndFilePath(req.u)
	if err != nil {
		return err
	}

	baseCmd := smbclientBaseCmd(req.u.User, hostPath, ".")
	// check directory exists in the smb shared folder and is a directory
	isDir, err := isDirectory(baseCmd, directory)
	if err != nil {
		return err
	}
	if !isDir {
		return fmt.Errorf("%s source path must be a directory", directory)
	}

	// download everything that's inside the directory (files and subdirectories)
	smbclientCmd := baseCmd + " --command 'prompt OFF;recurse ON; mget *'"

	if req.Dst != "" {
		_, err := os.Lstat(req.Dst)
		if err != nil {
			if os.IsNotExist(err) {
				// Create destination folder if it doesn't exists
				if err := os.MkdirAll(req.Dst, 0755); err != nil {
					return fmt.Errorf("failed to creat destination path: %s", err.Error())
				}
			} else {
				return err
			}
		}
	}

	_, err = runSmbClientCommand(smbclientCmd, req.Dst)
	return err
}

func (g *SmbGetter) GetFile(ctx context.Context, req *Request) error {
	if req.u.Host == "" || req.u.Path == "" {
		return fmt.Errorf(basePathError)
	}

	// If dst folder doesn't exists, we need to remove the created on later in case of failures
	dstExisted := false
	if req.Dst != "" {
		if _, err := os.Lstat(req.Dst); err == nil {
			dstExisted = true
		}
	}

	// First look in a possible local mount of the shared folder
	path := "/" + req.u.Host + req.u.Path
	if runtime.GOOS == "windows" {
		path = "/" + path
	}
	f := new(FileGetter)
	result := f.getFile(path, req, ctx)
	if result == nil {
		return nil
	}

	// If not mounted, try downloading the file using smbclient cli
	err := g.smbclientGetFile(req)
	if err == nil {
		return nil
	}

	result = multierror.Append(result, err)

	if !dstExisted {
		// Remove the destination created for smbclient
		os.Remove(req.Dst)
	}

	return fmt.Errorf("one of the options should be available: \n 1. local mount of the smb shared folder or; \n 2. smbclient cli installed. \n err: %s", result.Error())
}

func (g *SmbGetter) smbclientGetFile(req *Request) error {
	hostPath, filePath, err := findHostAndFilePath(req.u)
	if err != nil {
		return err
	}

	// Get file and subdirectory name when existent
	file := ""
	if strings.Contains(filePath, "/") {
		i := strings.LastIndex(filePath, "/")
		file = filePath[i+1:]
		filePath = filePath[:i]
	} else {
		file = filePath
		filePath = "."
	}

	baseCmd := smbclientBaseCmd(req.u.User, hostPath, filePath)
	// check file exists in the smb shared folder and is not a directory
	isDir, err := isDirectory(baseCmd, file)
	if err != nil {
		return err
	}
	if isDir {
		return fmt.Errorf("%s source path must be a file", file)
	}

	// download file
	smbclientCmd := baseCmd + " --command " + fmt.Sprintf("'get %s'", file)
	if req.Dst != "" {
		_, err := os.Lstat(req.Dst)
		if err != nil {
			if os.IsNotExist(err) {
				// Create destination folder if it doesn't exists
				if err := os.MkdirAll(filepath.Dir(req.Dst), 0755); err != nil {
					return fmt.Errorf("failed to creat destination path: %s", err.Error())
				}
			} else {
				return err
			}
		}
		smbclientCmd = baseCmd + " --command " + fmt.Sprintf("'get %s %s'", file, req.Dst)
	}
	_, err = runSmbClientCommand(smbclientCmd, "")
	return err
}

func smbclientBaseCmd(used *url.Userinfo, hostPath string, fileDir string) string {
	baseCmd := "smbclient -N"

	// Append auth user and password to baseCmd
	auth := used.Username()
	if auth != "" {
		if password, ok := used.Password(); ok {
			auth = auth + "%" + password
		}
		baseCmd = baseCmd + " -U " + auth
	}

	baseCmd = baseCmd + " " + hostPath + " --directory " + fileDir
	return baseCmd
}

func findHostAndFilePath(u *url.URL) (string, string, error) {
	// Host path
	hostPath := "//" + u.Host

	// Get shared directory
	path := strings.TrimPrefix(u.Path, "/")
	splt := regexp.MustCompile(`/`)
	directories := splt.Split(path, 2)

	if len(directories) > 0 {
		hostPath = hostPath + "/" + directories[0]
	}

	// Check file path
	if len(directories) <= 1 || directories[1] == "" {
		return "", "", fmt.Errorf("can not find file path and/or name in the smb url")
	}

	return hostPath, directories[1], nil
}

func isDirectory(baseCmd string, object string) (bool, error) {
	objectInfoCmd := baseCmd + " --command " + fmt.Sprintf("'allinfo %s'", object)
	output, err := runSmbClientCommand(objectInfoCmd, "")
	if err != nil {
		return false, err
	}
	if strings.Contains(output, "OBJECT_NAME_NOT_FOUND") {
		return false, fmt.Errorf("source path not found: %s", output)
	}
	return strings.Contains(output, "attributes: D"), nil
}

func runSmbClientCommand(smbclientCmd string, dst string) (string, error) {
	cmd := exec.Command("bash", "-c", smbclientCmd)

	if dst != "" {
		cmd.Dir = dst
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if err == nil {
		return buf.String(), nil
	}
	if exiterr, ok := err.(*exec.ExitError); ok {
		// The program has exited with an exit code != 0
		if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
			return buf.String(), fmt.Errorf(
				"%s exited with %d: %s",
				cmd.Path,
				status.ExitStatus(),
				buf.String())
		}
	}
	return buf.String(), fmt.Errorf("error running %s: %s", cmd.Path, buf.String())
}
