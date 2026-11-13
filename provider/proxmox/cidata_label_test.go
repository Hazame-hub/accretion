package proxmox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

// The NoCloud seed is byte-identical to the one every clone build so far was
// given (hash taken from the builder before the label became a parameter).
func TestTheNoCloudSeedIsUnchanged(t *testing.T) {
	iso, err := BuildNoCloudISO(map[string][]byte{
		"user-data": []byte("#cloud-config\nhostname: web-1\n"), "meta-data": []byte("instance-id: x\n"),
		"network-config": []byte(dhcpNetworkConfig),
	}, noCloudTime)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(iso)); got != "45cd2ca54e4fcdfa0ea6860ae4b27b9b5a850f1ff18186db354c1aaded179c78" || len(iso) != 57344 {
		t.Errorf("seed changed: sha256 %s, %d bytes", got, len(iso))
	}
}

// An answer disc carries the OEMDRV label in both volume descriptors and
// ks.cfg at its root, under its real name in the Joliet tree.
func TestAnAnswerDiscIsLabelledOEMDRV(t *testing.T) {
	ks := []byte("text\npoweroff\n")
	iso, err := BuildSeedISO(OEMDRVLabel, map[string][]byte{"ks.cfg": ks}, noCloudTime)
	if err != nil {
		t.Fatal(err)
	}
	pvd := iso[16*ISOSector : 17*ISOSector]
	if got := string(bytes.TrimRight(pvd[40:72], " ")); got != "OEMDRV" {
		t.Errorf("primary label %q", got)
	}
	svd := iso[17*ISOSector : 18*ISOSector]
	if !bytes.HasPrefix(svd[40:72], UCS2("OEMDRV")) {
		t.Errorf("Joliet label % x", svd[40:52])
	}
	if !bytes.Contains(iso, UCS2("ks.cfg")) || !bytes.Contains(iso, ks) {
		t.Error("ks.cfg is not on the disc")
	}
	if _, err := BuildSeedISO("oemdrv", map[string][]byte{"ks.cfg": ks}, noCloudTime); err == nil {
		t.Error("a lower-case label other than cidata was accepted")
	}
}
