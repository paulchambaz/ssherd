package daemon

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

func (s *Server) getMachines(w http.ResponseWriter, r *http.Request) {
	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load machines", http.StatusInternalServerError)
		log.Printf("Failed to load machines store: %v", err)
		return
	}
	if err := views.Machines(store).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getNewProxy(w http.ResponseWriter, r *http.Request) {
	if err := views.NewProxy().Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	portStr := strings.TrimSpace(r.FormValue("port"))
	if portStr == "" {
		portStr = "22"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "Invalid port", http.StatusBadRequest)
		return
	}

	protocolStr := strings.TrimSpace(r.FormValue("protocol"))
	if protocolStr == "" {
		protocolStr = "2"
	}
	protocol, err := strconv.Atoi(protocolStr)
	if err != nil || (protocol != 1 && protocol != 2) {
		http.Error(w, "Protocol must be 1 or 2", http.StatusBadRequest)
		return
	}

	id, err := internal.GenerateID()
	if err != nil {
		http.Error(w, "Failed to generate id", http.StatusInternalServerError)
		return
	}

	proxy := &internal.Proxy{
		ID:       id,
		Name:     strings.TrimSpace(r.FormValue("name")),
		Hostname: strings.TrimSpace(r.FormValue("hostname")),
		User:     strings.TrimSpace(r.FormValue("user")),
		Port:     port,
		Protocol: protocol,
		Password: r.FormValue("password"),
	}

	if proxy.Name == "" || proxy.Hostname == "" || proxy.User == "" {
		http.Error(w, "Name, hostname and user are required", http.StatusBadRequest)
		return
	}

	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load store", http.StatusInternalServerError)
		return
	}

	store.Proxies = append(store.Proxies, proxy)

	if err := internal.SaveMachinesStore(s.cfg.CachePath, store); err != nil {
		http.Error(w, "Failed to save store", http.StatusInternalServerError)
		log.Printf("Failed to save machines store: %v", err)
		return
	}

	http.Redirect(w, r, "/machines", http.StatusSeeOther)
}

func (s *Server) postDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load store", http.StatusInternalServerError)
		return
	}

	for _, m := range store.Machines {
		if m.ProxyID == id {
			http.Error(w, "Cannot delete proxy: machines still depend on it", http.StatusConflict)
			return
		}
	}

	filtered := store.Proxies[:0]
	for _, p := range store.Proxies {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == len(store.Proxies) {
		http.NotFound(w, r)
		return
	}
	store.Proxies = filtered

	if err := internal.SaveMachinesStore(s.cfg.CachePath, store); err != nil {
		http.Error(w, "Failed to save store", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/machines", http.StatusSeeOther)
}

func (s *Server) getNewMachines(w http.ResponseWriter, r *http.Request) {
	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load store", http.StatusInternalServerError)
		return
	}
	if err := views.NewMachines(store.Proxies).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postMachines(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	prefix := strings.TrimSpace(r.FormValue("name_prefix"))
	domainSuffix := strings.TrimSpace(r.FormValue("domain_suffix"))
	user := strings.TrimSpace(r.FormValue("user"))
	proxyID := strings.TrimSpace(r.FormValue("proxy_id"))
	suffixesRaw := r.FormValue("suffixes")

	if prefix == "" || user == "" {
		http.Error(w, "Prefix and user are required", http.StatusBadRequest)
		return
	}

	var suffixes []string
	for _, line := range strings.Split(suffixesRaw, "\n") {
		s := strings.TrimSpace(line)
		if s != "" {
			suffixes = append(suffixes, s)
		}
	}
	if len(suffixes) == 0 {
		http.Error(w, "At least one suffix is required", http.StatusBadRequest)
		return
	}

	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load store", http.StatusInternalServerError)
		return
	}

	if proxyID != "" && store.FindProxy(proxyID) == nil {
		http.Error(w, "Proxy not found", http.StatusBadRequest)
		return
	}

	var proxyProtocol int
	if proxyID != "" {
		proxy := store.FindProxy(proxyID)
		if proxy == nil {
			http.Error(w, "Proxy not found", http.StatusBadRequest)
			return
		}
		proxyProtocol = proxy.Protocol
	}

	var errs []string
	for _, suffix := range suffixes {
		name := prefix + suffix
		hostname := name
		if domainSuffix != "" {
			hostname = name + domainSuffix
		}

		id, err := internal.GenerateID()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: id generation failed", name))
			continue
		}

		m := &internal.Machine{
			ID:       id,
			Name:     name,
			Hostname: hostname,
			User:     user,
			Protocol: proxyProtocol,
			ProxyID:  proxyID,
		}
		store.Machines = append(store.Machines, m)
	}

	if err := internal.SaveMachinesStore(s.cfg.CachePath, store); err != nil {
		http.Error(w, "Failed to save store", http.StatusInternalServerError)
		log.Printf("Failed to save machines store: %v", err)
		return
	}

	if len(errs) > 0 {
		log.Printf("Partial errors during bulk machine creation: %v", errs)
	}

	http.Redirect(w, r, "/machines", http.StatusSeeOther)
}

func (s *Server) postDeleteMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load store", http.StatusInternalServerError)
		return
	}

	filtered := store.Machines[:0]
	for _, m := range store.Machines {
		if m.ID != id {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) == len(store.Machines) {
		http.NotFound(w, r)
		return
	}
	store.Machines = filtered

	if err := internal.SaveMachinesStore(s.cfg.CachePath, store); err != nil {
		http.Error(w, "Failed to save store", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/machines", http.StatusSeeOther)
}
