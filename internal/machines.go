package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	Port     int    `json:"port"`
	Protocol int    `json:"protocol"`
	Password string `json:"password"`
}

type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	Protocol int    `json:"protocol"`
	ProxyID  string `json:"proxy_id,omitempty"`
}

type MachinesStore struct {
	Proxies  []*Proxy   `json:"proxies"`
	Machines []*Machine `json:"machines"`
}

func (s *MachinesStore) FindProxy(id string) *Proxy {
	for _, p := range s.Proxies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *MachinesStore) FindMachine(id string) *Machine {
	for _, m := range s.Machines {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func machinesFile(cachePath string) string {
	return filepath.Join(cachePath, "machines.json")
}

func LoadMachinesStore(cachePath string) (*MachinesStore, error) {
	data, err := os.ReadFile(machinesFile(cachePath))
	if os.IsNotExist(err) {
		return &MachinesStore{Proxies: []*Proxy{}, Machines: []*Machine{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read machines.json: %w", err)
	}
	var store MachinesStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("cannot parse machines.json: %w", err)
	}
	if store.Proxies == nil {
		store.Proxies = []*Proxy{}
	}
	if store.Machines == nil {
		store.Machines = []*Machine{}
	}
	return &store, nil
}

func SaveMachinesStore(cachePath string, store *MachinesStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot marshal machines store: %w", err)
	}
	if err := os.WriteFile(machinesFile(cachePath), data, 0644); err != nil {
		return fmt.Errorf("cannot write machines.json: %w", err)
	}
	return nil
}
