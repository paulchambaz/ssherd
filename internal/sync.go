package internal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
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

// CopyLocalToRemote copie localDir vers remoteDir sur la machine distante.
// Symétrique de SyncDirToLocal : tar local → base64 → SSH → décode + extrait.
// Seuls les fichiers absents ou plus anciens sur le remote sont transférés.
func (c *Client) CopyLocalToRemote(localDir, remoteDir string) error {
	if _, err := os.Stat(localDir); os.IsNotExist(err) {
		log.Printf("ssh: [%s] CopyLocalToRemote: %s absent, skip", c.name, localDir)
		return nil
	}

	// Lister les fichiers locaux.
	var localFiles []RemoteFileInfo
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(localDir, path)
		localFiles = append(localFiles, RemoteFileInfo{
			Path:    rel,
			ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk local dir: %w", err)
	}
	if len(localFiles) == 0 {
		log.Printf("ssh: [%s] CopyLocalToRemote: %s empty, skip", c.name, localDir)
		return nil
	}

	// Interroger les mtimes remote pour ne transférer que ce qui est nécessaire.
	remoteFiles, err := c.ListRemoteFiles(remoteDir)
	if err != nil {
		return fmt.Errorf("list remote files: %w", err)
	}
	remoteMtimes := make(map[string]time.Time, len(remoteFiles))
	for _, rf := range remoteFiles {
		remoteMtimes[rf.Path] = rf.ModTime
	}

	var toSync []string
	for _, lf := range localFiles {
		remoteMtime, exists := remoteMtimes[lf.Path]
		if !exists || lf.ModTime.After(remoteMtime) {
			toSync = append(toSync, lf.Path)
		}
	}

	if len(toSync) == 0 {
		log.Printf("ssh: [%s] CopyLocalToRemote: %s — %d files, all up to date", c.name, localDir, len(localFiles))
		return nil
	}

	log.Printf("ssh: [%s] CopyLocalToRemote: %s — syncing %d/%d files", c.name, localDir, len(toSync), len(localFiles))

	// Construire le tar en local à partir de la liste de fichiers à transférer.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, rel := range toSync {
		absPath := filepath.Join(localDir, rel)
		info, err := os.Stat(absPath)
		if err != nil {
			log.Printf("ssh: CopyLocalToRemote: stat %s: %v", absPath, err)
			continue
		}
		hdr := &tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %s: %w", rel, err)
		}
		f, err := os.Open(absPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", absPath, err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return fmt.Errorf("tar write %s: %w", rel, err)
		}
		f.Close()
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("gzip close: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Envoyer via stdin pour éviter les limites de longueur de commande SSH.
	cmd := fmt.Sprintf(
		"mkdir -p %s && base64 -d | tar -xzf - -C %s",
		shellEscape(remoteDir),
		shellEscape(remoteDir),
	)
	_, stderr, code, err := c.RunWithStdin(cmd, []byte(encoded))
	if err != nil {
		return fmt.Errorf("remote extract: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("remote extract failed (code=%d): %s", code, stderr)
	}

	log.Printf("ssh: [%s] CopyLocalToRemote: %s — done (%d files, %d bytes)", c.name, localDir, len(toSync), buf.Len())
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
