package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// labStorageServer answers like flk-vega-01 did on 2026-11-14: root
// filesystem 94 GB with 4.2 GB left, the `local` dir storage on that same
// filesystem holding sparse VM disks, plus a shared NFS storage seen from two
// nodes. refuse names paths answered with a 403 (value: the privilege it
// stands for, for the reader).
func labStorageServer(t *testing.T, refuse map[string]string) (*Client, *[]string) {
	t.Helper()
	var asked []string
	const gb = 1000 * 1000 * 1000
	rootTotal, rootUsed, rootAvail := int64(94*gb), int64(85*gb), int64(42*gb/10)
	bodies := map[string]string{
		"/nodes": `[{"node":"flk-vega-01","status":"online"},{"node":"flk-vega-02","status":"online"},{"node":"flk-vega-03","status":"offline"}]`,
		"/nodes/flk-vega-01/status": fmt.Sprintf(`{"rootfs":{"total":%d,"used":%d,"avail":%d,"free":%d}}`,
			rootTotal, rootUsed, rootAvail, rootTotal-rootUsed),
		"/nodes/flk-vega-01/storage": fmt.Sprintf(`[
			{"storage":"local","type":"dir","content":"iso,vztmpl,backup,images","active":1,"enabled":1,"shared":0,"total":%d,"used":%d,"avail":%d},
			{"storage":"nas","type":"nfs","content":"backup","active":1,"enabled":1,"shared":1,"total":1000,"used":10,"avail":990},
			{"storage":"off","type":"dir","content":"images","active":0,"enabled":0,"shared":0,"total":0,"used":0,"avail":0}
		]`, rootTotal, rootUsed+1000, rootAvail),
		"/nodes/flk-vega-01/storage/local/content": `[
			{"volid":"local:203/vm-203-disk-2.raw","vmid":203,"format":"raw","size":214748364800,"used":2400000000},
			{"volid":"local:102/vm-102-disk-1.qcow2","vmid":"102","format":"qcow2","size":21474836480,"used":1800000000},
			{"volid":"local:115/vm-115-disk-0.qcow2","vmid":115,"format":"qcow2","size":540672}
		]`,
		"/nodes/flk-vega-01/storage/local/rrddata": `[{"time":1794600000,"used":80000000000,"total":94000000000},{"time":1794596400,"used":79000000000,"total":94000000000},{"time":1794603600}]`,
		"/nodes/flk-vega-01/storage/nas/rrddata":   `[]`,
		"/nodes/flk-vega-02/status":                `{"rootfs":{"total":10,"used":5,"avail":5}}`,
		"/nodes/flk-vega-02/storage":               `[{"storage":"nas","type":"nfs","content":"backup","active":1,"shared":1,"total":1000,"used":10,"avail":990}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: capacity reading must only GET", r.Method, r.URL.Path)
		}
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		asked = append(asked, p+"?"+r.URL.RawQuery)
		if _, ok := refuse[p]; ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"data":null}`))
			return
		}
		b, ok := bodies[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + b + `}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "PVEAPIToken=t", HTTP: srv.Client()}, &asked
}

func TestStorageCapacityReadsTheLabNode(t *testing.T) {
	c, asked := labStorageServer(t, nil)
	got, err := c.StorageCapacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.Node != "flk-vega-01" || n.RootFS == nil || n.RootFS.Avail != 4200000000 || n.RootFS.Total != 94000000000 {
		t.Fatalf("rootfs = %+v (err %v)", n.RootFS, n.RootFSErr)
	}
	if len(n.Storages) != 3 {
		t.Fatalf("storages = %d, want 3", len(n.Storages))
	}
	local := n.Storages[0]
	if local.Storage != "local" || !local.HoldsImages() || !local.VolumesRead || len(local.Volumes) != 3 {
		t.Fatalf("local = %+v", local)
	}
	if v := local.Volumes[2]; v.VolID != "local:203/vm-203-disk-2.raw" || v.VMID != 203 || v.Size != 214748364800 || v.Used != 2400000000 {
		t.Errorf("203 volume = %+v", v)
	}
	if v := local.Volumes[0]; v.VMID != 102 {
		t.Errorf("a vmid sent as a string must still read, got %+v", v)
	}
	if v := local.Volumes[1]; v.Used != -1 {
		t.Errorf("a volume with no used must read -1 (not said), got %d", v.Used)
	}
	if len(local.History) != 2 || !local.History[0].Time.Before(local.History[1].Time) {
		t.Errorf("history = %+v, want 2 points oldest first (the step with no value dropped)", local.History)
	}
	if off := n.Storages[2]; off.Active || off.Enabled || off.VolumesRead || off.History != nil {
		t.Errorf("an inactive storage is listed, not read: %+v", off)
	}
	nas := n.Storages[1]
	if len(nas.SeenFrom) != 2 || nas.SeenFrom[1] != "flk-vega-02" {
		t.Errorf("shared storage seen from = %v, want both nodes", nas.SeenFrom)
	}
	if len(got.Nodes[1].Storages) != 0 {
		t.Errorf("a shared storage must be counted once, node 2 has %+v", got.Nodes[1].Storages)
	}
	if !got.Nodes[2].Offline || got.Nodes[2].RootFS != nil {
		t.Errorf("offline node must be listed with nothing read: %+v", got.Nodes[2])
	}
	for _, a := range *asked {
		if strings.Contains(a, "flk-vega-03") {
			t.Errorf("asked an offline node: %s", a)
		}
		if strings.HasSuffix(strings.SplitN(a, "?", 2)[0], "/content") && !strings.Contains(a, "content=images") {
			t.Errorf("content listing must ask for images only: %s", a)
		}
	}
}

func TestStorageCapacityKeepsRefusalsOnTheirPart(t *testing.T) {
	c, _ := labStorageServer(t, map[string]string{
		"/nodes/flk-vega-01/status":                "Sys.Audit",
		"/nodes/flk-vega-01/storage/local/content": "Datastore.Audit",
	})
	got, err := c.StorageCapacity(context.Background())
	if err != nil {
		t.Fatalf("a refused part must not fail the reading: %v", err)
	}
	n := got.Nodes[0]
	if n.RootFS != nil || n.RootFSErr == nil {
		t.Errorf("refused rootfs must be nil with its error, got %+v / %v", n.RootFS, n.RootFSErr)
	}
	local := n.Storages[0]
	if local.VolumesRead || local.VolumesErr == nil || local.Volumes != nil {
		t.Errorf("refused content must be unread with its error, got %+v", local)
	}
	if local.Total == 0 || len(local.History) == 0 {
		t.Errorf("the rest of the storage must still be read: %+v", local)
	}
}

func TestStorageCapacityNeedsTheNodeList(t *testing.T) {
	c, _ := labStorageServer(t, map[string]string{"/nodes": "Sys.Audit"})
	if _, err := c.StorageCapacity(context.Background()); err == nil {
		t.Fatal("no node list, no reading: want an error")
	}
}
