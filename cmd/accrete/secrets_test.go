package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

const secretValue = "s3cret-db-pass"

// A clone whose bootstrap writes a file holding a secret.
const withSecret = `        bootstrap:
          files:
            - path: /etc/app/db.conf
              content: "password={{secret.db-pass}}"
`

// accreteEnv runs one command with env as the extra environment, and checks
// the secret reached neither the output nor the state.
func (l *applyLab) accreteEnv(env map[string]string, args ...string) (string, int) {
	l.t.Helper()
	var out bytes.Buffer
	args = append(args, "-f", l.manifest)
	code := run(context.Background(), args, strings.NewReader(""), &out, &out, func(k string) string {
		if k == applyTokenVar {
			return pvetest.Token
		}
		return env[k]
	})
	if strings.Contains(out.String(), secretValue) {
		l.t.Fatalf("the secret reached the output:\n%s", out.String())
	}
	if b, err := os.ReadFile(l.state); err == nil && bytes.Contains(b, []byte(secretValue)) {
		l.t.Fatalf("the secret reached the state")
	}
	return out.String(), code
}

func TestApplyPutsASecretInForTheFirstBootOnly(t *testing.T) {
	l := newApplyLab(t, withSecret)
	shorten(t)
	env := map[string]string{"ACCRETE_SECRET_DB_PASS": secretValue}
	out, code := l.accreteEnv(env, "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "started for its first boot, which powers it off once cloud-init is done",
		"its first boot ran cloud-init and powered it off",
		"replaced its seed with local:iso/accrete-", "started again", "confirmed and recorded", "2 done, 0 failed")

	// What is left on storage is the seed without the secret, under the
	// same instance id; nothing anywhere holds the value.
	seeds := 0
	for volid, b := range l.pve.Files {
		if bytes.Contains(b, []byte(secretValue)) {
			t.Errorf("%s still holds the secret", volid)
		}
		if bytes.Contains(b, []byte("password=(removed by accrete after the first boot)")) {
			seeds++
			if !bytes.Contains(b, []byte("instance-id: ")) {
				t.Errorf("%s lost its instance id", volid)
			}
		}
	}
	if seeds != 2 {
		t.Errorf("want the 2 machines' seeds without the secret, found %d among %d files", seeds, len(l.pve.Files))
	}
	for _, vmid := range []int{100, 102} {
		if g := l.pve.Guest(vmid); g == nil || g.Status != "running" || !strings.HasPrefix(g.Config["ide2"].(string), "local:iso/accrete-") {
			t.Errorf("guest %d: %+v", vmid, g)
		}
	}
	if p := l.plan(); !strings.Contains(p.Summary, "2 unchanged") {
		t.Errorf("after apply: %s", p.Summary)
	}
}

func TestApplyRefusesASecretThatIsNotSet(t *testing.T) {
	l := newApplyLab(t, withSecret)
	out, code := l.accreteEnv(nil, "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "set ACCRETE_SECRET_DB_PASS in the environment") || !strings.Contains(out, "Nothing was sent.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if w := l.pve.Writes(); len(w) != 0 {
		t.Errorf("writes %v", w)
	}
}

func TestAResizeNeedsNoSecret(t *testing.T) {
	l := newApplyLab(t, withSecret)
	shorten(t)
	env := map[string]string{"ACCRETE_SECRET_DB_PASS": secretValue}
	if out, code := l.accreteEnv(env, "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	writeFile(t, l.manifest, strings.Replace(readFile(t, l.manifest), "memory_mb: 1024", "memory_mb: 2048", 1))
	if out, code := l.accreteEnv(nil, "apply", "--auto-approve"); code != 0 || !strings.Contains(out, "change web/0") {
		t.Fatalf("resize without the secret set: exit %d\n%s", code, out)
	}
}

func TestSecretEnvName(t *testing.T) {
	for in, want := range map[string]string{"db-pass": "ACCRETE_SECRET_DB_PASS", "api.token": "ACCRETE_SECRET_API_TOKEN", "X9": "ACCRETE_SECRET_X9"} {
		if got := secretEnv(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// noSecretLeft fails when any file on the storage holds the secret, as is or
// base64-encoded (an Ignition file's contents).
func noSecretLeft(t *testing.T, l *applyLab) {
	t.Helper()
	for volid, b := range l.pve.Files {
		if holdsSecret(b) {
			t.Errorf("%s still holds the secret", volid)
		}
	}
}

// holdsSecret says whether b holds the secret, as is or base64-encoded (an
// Ignition file's contents, a kickstart's files).
func holdsSecret(b []byte) bool {
	enc := base64.StdEncoding.EncodeToString([]byte("password=" + secretValue))
	return bytes.Contains(b, []byte(secretValue)) || bytes.Contains(b, []byte(enc))
}

func TestAnInstallTakesASecretOnItsAnswerDiscOnly(t *testing.T) {
	l := newInstallLab(t, "          files: [{path: /etc/app/db.conf, content: \"password={{secret.db-pass}}\"}]\n")
	var answer []byte
	l.pve.OnUpload = func(volid string, body []byte) {
		if holdsSecret(body) {
			answer = body
		}
	}
	out, code := l.accreteEnv(map[string]string{"ACCRETE_SECRET_DB_PASS": secretValue}, "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "answer disc deleted")
	if answer == nil {
		t.Error("the answer disc never carried the secret")
	}
	noSecretLeft(t, l)
}

func TestAnImageBuildReplacesItsSeedAfterTheFirstBoot(t *testing.T) {
	l := newImageLab(t, imageManifest, "          files: [{path: /etc/app/db.conf, content: \"password={{secret.db-pass}}\"}]\n")
	out, code := l.accreteEnv(map[string]string{"ACCRETE_SECRET_DB_PASS": secretValue}, "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "its first boot applied its ignition config and powered it off",
		"replaced its seed with local:iso/accrete-", "started again", "1 done, 0 failed")
	noSecretLeft(t, l)
	redactedEnc := base64.StdEncoding.EncodeToString([]byte("password=" + redacted))
	found := false
	for _, b := range l.pve.Files {
		found = found || bytes.Contains(b, []byte(redactedEnc))
	}
	if !found {
		t.Error("no seed without the secret is left for the machine")
	}
}
