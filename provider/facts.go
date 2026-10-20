package provider

// Facts is not a second facts schema. It is the subset of the asset facts
// model a hypervisor can know, with the same JSON names and units the asset
// columns use, so a hypervisor's "4 vCPU / 8 GiB" and an agent's
// runtime.NumCPU / mem.Total land in fields with the same name and one
// comparator can read both. In Sybil Continuum,
// repository.TestFactsIsAProjectionOfTheAssetModel
// holds this true: every tag here names an asset field of the same shape.
type Facts struct {
	CPUCores          int32       `json:"cpu_cores"`
	RAMBytes          int64       `json:"ram_bytes"`
	OS                string      `json:"os"`
	Disks             []Disk      `json:"disks"`
	NetworkInterfaces []Interface `json:"network_interfaces"`
}

// Disk is what a hypervisor knows about a virtual disk: its size. Mount,
// used and filesystem are the guest's to report; they stay zero here and the
// comparator never reads them from this side.
type Disk struct {
	Mount  string `json:"mount"`
	Total  uint64 `json:"total"`
	Used   uint64 `json:"used"`
	FsType string `json:"fs_type"`
}

// Interface is a NIC as the hypervisor configured it. HardwareAddr is the
// MAC; Addresses come from a guest agent when one runs.
type Interface struct {
	Name         string   `json:"name"`
	Addresses    []string `json:"addresses"`
	HardwareAddr string   `json:"hardware_addr"`
}
