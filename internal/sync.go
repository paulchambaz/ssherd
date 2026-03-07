package internal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RemoteFileInfo contient le chemin relatif et la mtime d'un fichier remote.
type RemoteFileInfo struct {
	Path    string
	ModTime time.Time
}

// ListRemoteFiles retourne la liste des fichiers sous remoteDir avec leurs mtimes.
// Utilise find -printf pour obtenir le timestamp Unix flottant et le chemin relatif.
func (c *Client) ListRemoteFiles(remoteDir string) ([]RemoteFileInfo, error) {
	out, _, code, err := c.Run(fmt.Sprintf(
		"find %s -type f -printf '%%T@ %%P\n' 2>/dev/null",
		shellEscape(remoteDir),
	))
	if err != nil {
		return nil, fmt.Errorf("find: %w", err)
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var files []RemoteFileInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		ts, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			continue
		}
		sec := int64(ts)
		nsec := int64((ts - float64(sec)) * 1e9)
		files = append(files, RemoteFileInfo{
			Path:    parts[1],
			ModTime: time.Unix(sec, nsec),
		})
	}
	return files, nil
}

// SyncDirToLocal transfère depuis remoteDir vers localDir les fichiers dont
// la mtime remote est plus récente que la mtime locale, ou qui n'existent pas
// localement. Utilise tar via la connexion SSH existante — aucun binaire externe.
func (c *Client) SyncDirToLocal(remoteDir, localDir string) error {
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("mkdir local: %w", err)
	}

	remoteFiles, err := c.ListRemoteFiles(remoteDir)
	if err != nil {
		return fmt.Errorf("list remote files: %w", err)
	}
	if len(remoteFiles) == 0 {
		log.Printf("ssh: [%s] SyncDirToLocal: %s — empty or absent", c.name, remoteDir)
		return nil
	}

	// Filtrer les fichiers qui ont besoin d'être transférés.
	var toSync []string
	for _, rf := range remoteFiles {
		localPath := filepath.Join(localDir, rf.Path)
		info, err := os.Stat(localPath)
		if os.IsNotExist(err) || err != nil {
			toSync = append(toSync, rf.Path)
			continue
		}
		if rf.ModTime.After(info.ModTime()) {
			toSync = append(toSync, rf.Path)
		}
	}

	if len(toSync) == 0 {
		log.Printf("ssh: [%s] SyncDirToLocal: %s — %d files, all up to date", c.name, remoteDir, len(remoteFiles))
		return nil
	}

	log.Printf("ssh: [%s] SyncDirToLocal: %s — syncing %d/%d files", c.name, remoteDir, len(toSync), len(remoteFiles))

	// Passer la liste de fichiers au remote via base64 pour éviter les
	// problèmes d'escaping et les limites de longueur de commande.
	fileList := strings.Join(toSync, "\n")
	encodedList := base64.StdEncoding.EncodeToString([]byte(fileList))

	// Construire le tar remote : décode la liste, tar les fichiers sélectionnés,
	// encode le résultat en base64 pour le transit via stdout SSH.
	cmd := fmt.Sprintf(
		"printf '%%s' %s | base64 -d | tar -czf - -C %s --files-from=- 2>/dev/null | base64 -w0",
		shellEscape(encodedList),
		shellEscape(remoteDir),
	)

	stdout, _, code, err := c.Run(cmd)
	if err != nil {
		return fmt.Errorf("remote tar: %w", err)
	}
	if strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("remote tar produced no output (code=%d)", code)
	}

	tarData, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		return fmt.Errorf("base64 decode: %w", err)
	}

	if err := extractTarGz(tarData, localDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	log.Printf("ssh: [%s] SyncDirToLocal: %s — done (%d files transferred, %d bytes)", c.name, remoteDir, len(toSync), len(tarData))
	return nil
}

// extractTarGz extrait une archive tar.gz dans destDir en préservant les mtimes.
func extractTarGz(data []byte, destDir string) error {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	cleanDest := filepath.Clean(destDir)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		target := filepath.Join(destDir, hdr.Name)

		// Sécurité : path traversal.
		if !strings.HasPrefix(filepath.Clean(target), cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("path traversal: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent: %w", err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			f.Close()
			// Préserver la mtime remote pour que le prochain appel à
			// SyncDirToLocal puisse comparer correctement.
			if err := os.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
				log.Printf("ssh: extractTarGz: chtimes %s: %v", target, err)
			}
		}
	}
	return nil
}
