package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// extractBinary extracts the sonar binary from an archive and returns the path to the extracted file.
func extractBinary(archivePath string) (string, error) {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractBinaryZip(archivePath)
	}
	return extractBinaryTarGz(archivePath)
}

// extractBinaryZip extracts the sonar binary from a .zip archive.
func extractBinaryZip(zipPath string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("invalid archive: %w", err)
	}
	defer r.Close()

	binaryName := "sonar"
	if runtime.GOOS == "windows" {
		binaryName = "sonar.exe"
	}

	for _, f := range r.File {
		if filepath.Base(f.Name) == binaryName && !f.FileInfo().IsDir() {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()

			tmpBin, err := os.CreateTemp("", "sonar-bin-*")
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(tmpBin, rc); err != nil {
				tmpBin.Close()
				os.Remove(tmpBin.Name())
				return "", err
			}
			tmpBin.Close()
			os.Chmod(tmpBin.Name(), 0755)
			return tmpBin.Name(), nil
		}
	}

	return "", fmt.Errorf("sonar binary not found in archive")
}

// extractBinaryTarGz extracts the sonar binary from a .tar.gz archive.
func extractBinaryTarGz(tarPath string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("invalid archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading archive: %w", err)
		}

		base := filepath.Base(hdr.Name)
		if (base == "sonar" || base == "sonar.exe") && hdr.Typeflag == tar.TypeReg {
			tmpBin, err := os.CreateTemp("", "sonar-bin-*")
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(tmpBin, tr); err != nil {
				tmpBin.Close()
				os.Remove(tmpBin.Name())
				return "", err
			}
			tmpBin.Close()
			os.Chmod(tmpBin.Name(), 0755)
			return tmpBin.Name(), nil
		}
	}

	return "", fmt.Errorf("sonar binary not found in archive")
}

// replaceBinary atomically replaces oldPath with newPath.
func replaceBinary(oldPath, newPath string) error {
	// Rename is atomic on the same filesystem.
	// Since temp may be on a different fs, copy + rename instead.
	tmpDest := oldPath + ".new"
	src, err := os.Open(newPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(tmpDest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", tmpDest, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpDest)
		return err
	}
	dst.Close()

	if runtime.GOOS == "windows" {
		// Windows cannot rename over a running executable.
		// Move the old binary out of the way first.
		oldBackup := oldPath + ".old"
		os.Remove(oldBackup) // clean up any previous failed update
		if err := os.Rename(oldPath, oldBackup); err != nil {
			os.Remove(tmpDest)
			return fmt.Errorf("cannot replace binary: %w", err)
		}
		if err := os.Rename(tmpDest, oldPath); err != nil {
			// Try to restore the old binary
			os.Rename(oldBackup, oldPath)
			os.Remove(tmpDest)
			return fmt.Errorf("cannot replace binary: %w", err)
		}
		// Clean up old binary (may fail if still locked, that's ok)
		os.Remove(oldBackup)
		return nil
	}

	if err := os.Rename(tmpDest, oldPath); err != nil {
		os.Remove(tmpDest)
		return fmt.Errorf("cannot replace binary: %w", err)
	}
	return nil
}
