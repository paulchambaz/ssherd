package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

// getSharedVisualization renders a simplified shareable page for a visualization
func (s *Server) getSharedVisualization(w http.ResponseWriter, r *http.Request) {
	vizID := r.PathValue("viz_id")
	if vizID == "" {
		http.NotFound(w, r)
		return
	}

	// Find the project and visualization
	// We need to search through all projects since we only have the viz ID
	projects, err := internal.LoadProjects(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load projects", http.StatusInternalServerError)
		log.Printf("Failed to load projects: %v", err)
		return
	}

	var project *internal.Project
	var viz *internal.Visualization

	for _, p := range projects {
		v, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, vizID)
		if err == nil {
			project = p
			viz = v
			break
		}
	}

	if project == nil || viz == nil {
		http.NotFound(w, r)
		return
	}

	if err := views.ShareVisualizationPage(project, viz).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render share page: %v", err)
	}
}

// getSharedFile serves a shared image by filename from ~/.cache/ssherd/share/
func (s *Server) getSharedFile(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	if filename == "" {
		http.NotFound(w, r)
		return
	}

	// Security: prevent directory traversal
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	shareDir := filepath.Join(s.cfg.CachePath, "share")
	filePath := filepath.Join(shareDir, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ext := filepath.Ext(filename)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000") // Cache for 1 year
	w.Write(data)
}

// postShareVisualization creates a shareable link for the current visualization combo
func (s *Server) postShareVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	viz, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	// Build selection from query params (same logic as getVisualizationFile)
	selection := viz.DefaultSelection()
	for i, ax := range viz.ToggleableAxes() {
		paramName := ax.Name
		if paramName == "" {
			paramName = fmt.Sprintf("axis%d", i)
		}

		if val := r.FormValue(paramName); val != "" {
			for _, v := range ax.Values {
				if v == val {
					selection[paramName] = val
					break
				}
			}
		}
	}

	// Get the source file paths
	localRepoDir := filepath.Join(s.cfg.CachePath, p.ID, "repo")
	sourcePathSVG := viz.ResolveOutputPath(localRepoDir, selection)
	sourcePathPNG := internal.VizLocalPNGPath(sourcePathSVG)

	// Check if SVG exists
	svgExists := false
	if _, err := os.Stat(sourcePathSVG); err == nil {
		svgExists = true
	}

	// Check if PNG exists
	pngExists := false
	if _, err := os.Stat(sourcePathPNG); err == nil {
		pngExists = true
	}

	// At least one must exist
	if !svgExists && !pngExists {
		http.Error(w, "Visualization file not found", http.StatusNotFound)
		return
	}

	// Generate UUID for the shared files
	uuid, err := generateUUID()
	if err != nil {
		http.Error(w, "Failed to generate UUID", http.StatusInternalServerError)
		log.Printf("Failed to generate UUID: %v", err)
		return
	}

	// Create share directory if it doesn't exist
	shareDir := filepath.Join(s.cfg.CachePath, "share")
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		http.Error(w, "Failed to create share directory", http.StatusInternalServerError)
		log.Printf("Failed to create share directory: %v", err)
		return
	}

	// Helper function to copy a file
	copyFile := func(src, dst string) error {
		source, err := os.Open(src)
		if err != nil {
			return err
		}
		defer source.Close()

		dest, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer dest.Close()

		_, err = io.Copy(dest, source)
		return err
	}

	// Copy both SVG and PNG with the same UUID
	primaryExt := ".svg"
	if svgExists {
		destPath := filepath.Join(shareDir, uuid+".svg")
		if err := copyFile(sourcePathSVG, destPath); err != nil {
			http.Error(w, "Failed to copy SVG file", http.StatusInternalServerError)
			log.Printf("Failed to copy SVG: %v", err)
			return
		}
	} else {
		primaryExt = ".png"
	}

	if pngExists {
		destPath := filepath.Join(shareDir, uuid+".png")
		if err := copyFile(sourcePathPNG, destPath); err != nil {
			// PNG copy is non-fatal, just log it
			log.Printf("Failed to copy PNG: %v", err)
		}
	}

	// Return the primary shareable URL (SVG preferred, PNG as fallback)
	sharedFilename := uuid + primaryExt
	shareURL := fmt.Sprintf("/share/file/%s", sharedFilename)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"url": "%s", "filename": "%s"}`, shareURL, sharedFilename)
}

// generateUUID generates a random UUID-like string (32 hex characters)
func generateUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
