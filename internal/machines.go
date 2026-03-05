package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// GPUInfo décrit un modèle de GPU connu.
type GPUInfo struct {
	Model   string
	VRAMMiB int
	TFLOPs  float64 // FP32 approximatif, pour trier par puissance
}

// KnownGPUs est la liste des GPUs reconnus par le scheduler.
// Utilisée pour la validation et pour trier les machines par préférence.
var KnownGPUs = []GPUInfo{
	{Model: "RTX A4500", VRAMMiB: 20480, TFLOPs: 31.7},
	{Model: "RTX 4090", VRAMMiB: 24576, TFLOPs: 82.6},
	{Model: "RTX 4080", VRAMMiB: 16384, TFLOPs: 48.7},
	{Model: "RTX 4070 Ti", VRAMMiB: 12288, TFLOPs: 40.0},
	{Model: "RTX 4070", VRAMMiB: 12288, TFLOPs: 29.1},
	{Model: "RTX 4060 Ti", VRAMMiB: 8192, TFLOPs: 22.1},
	{Model: "RTX 4060", VRAMMiB: 8192, TFLOPs: 15.1},
	{Model: "RTX 4050", VRAMMiB: 6144, TFLOPs: 9.6},
	{Model: "RTX 3090", VRAMMiB: 24576, TFLOPs: 35.6},
	{Model: "RTX 3080", VRAMMiB: 10240, TFLOPs: 29.8},
	{Model: "RTX 3070", VRAMMiB: 8192, TFLOPs: 20.4},
	{Model: "RTX 3060", VRAMMiB: 12288, TFLOPs: 12.7},
	{Model: "RTX 2080 Ti", VRAMMiB: 11264, TFLOPs: 13.4},
	{Model: "RTX 2080", VRAMMiB: 8192, TFLOPs: 10.1},
	{Model: "RTX 2070", VRAMMiB: 8192, TFLOPs: 7.5},
	{Model: "RTX 2060", VRAMMiB: 6144, TFLOPs: 6.5},
	{Model: "GTX 1080 Ti", VRAMMiB: 11264, TFLOPs: 11.3},
	{Model: "GTX 1080", VRAMMiB: 8192, TFLOPs: 8.9},
	{Model: "GTX 1070", VRAMMiB: 8192, TFLOPs: 6.5},
}

func FindGPU(model string) (GPUInfo, bool) {
	for _, g := range KnownGPUs {
		if g.Model == model {
			return g, true
		}
	}
	return GPUInfo{}, false
}

// MachineStatus représente l'état de disponibilité d'une machine.
type MachineStatus string

const (
	MachineStatusUnknown     MachineStatus = "unknown"
	MachineStatusAvailable   MachineStatus = "available"
	MachineStatusBusy        MachineStatus = "busy"
	MachineStatusDeprecated  MachineStatus = "deprecated" // GPU incompatible ou absent
	MachineStatusUnreachable MachineStatus = "unreachable"
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
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Hostname string        `json:"hostname"`
	User     string        `json:"user"`
	Protocol int           `json:"protocol"`
	ProxyID  string        `json:"proxy_id,omitempty"`
	GPUModel string        `json:"gpu_model,omitempty"`
	Status   MachineStatus `json:"status,omitempty"`
}

// VRAMMiB retourne la VRAM connue de la machine, ou 0 si le GPU est inconnu.
func (m *Machine) VRAMMiB() int {
	if g, ok := FindGPU(m.GPUModel); ok {
		return g.VRAMMiB
	}
	return 0
}

// SatisfiesRequirements vérifie que la machine est compatible avec les besoins du job.
func (m *Machine) SatisfiesRequirements(req GPURequirements) bool {
	if m.Status == MachineStatusDeprecated {
		return false
	}
	if req.MinVRAMMB > 0 && m.VRAMMiB() > 0 && m.VRAMMiB() < req.MinVRAMMB/1024 {
		return false
	}
	if req.PreferredGPU != "" && req.PreferredGPU != "any" && m.GPUModel != req.PreferredGPU {
		// C'est une préférence, pas un critère éliminatoire — on filtre soft ici,
		// le dispatcher l'utilisera comme tri.
	}
	return true
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
