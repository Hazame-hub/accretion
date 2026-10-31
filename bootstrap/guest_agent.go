package bootstrap

// Letting the hypervisor read files on the guest.
//
// Reading a file through the hypervisor (Proxmox's agent/file-read) uses the
// guest agent's file commands, and RHEL-family images (Rocky, Alma, RHEL, CentOS Stream) ship qemu-guest-agent
// with exactly those commands turned off in /etc/sysconfig/qemu-ga: either
// an allow-list that leaves them out (--allow-rpcs=…) or a block-list that
// names them (BLACKLIST_RPC=…). The Proxmox privilege then makes no
// difference; the agent answers "Command guest-file-open has been disabled".
//
// Only the three commands a read needs are let through: open, read and
// close. Writing, seeking and guest-exec stay as the image left them, so a
// token with VM.GuestAgent.FileRead can read files and do nothing more.
// Images without that file (Debian, Ubuntu) block nothing and are left alone;
// the agent is restarted only when the file was changed.
//
// SELinux is the second lock. On an enforcing RHEL-family guest the agent
// runs confined (virt_qemu_ga_t), and the policy refuses it ordinary files
// even with the commands allowed: the read fails "Permission denied". The
// boolean virt_qemu_ga_read_nonsecurity_files lets it read files that are not
// security files (shadow, keys and the like stay refused), which is what the
// FileRead privilege is meant to grant. It is turned on only where SELinux
// tools exist and it is off; it is a read permission and nothing more.

// QemuGAConfig is the agent's settings file on RHEL-family guests.
const QemuGAConfig = "/etc/sysconfig/qemu-ga"

// guestAgentFileRPCs are the agent commands a guest file read uses.
const guestAgentFileRPCs = "guest-file-open,guest-file-read,guest-file-close"

// guestAgentReadBool is the SELinux boolean that lets the confined agent read
// files that are not security files.
const guestAgentReadBool = "virt_qemu_ga_read_nonsecurity_files"

// guestAgentFilesScript is run as `sh -c guestAgentFilesScript
// guest-agent-files RPCS BOOL`. It never fails the boot: a guest it cannot fix is
// left exactly as unreadable as it was.
// ACCRETION_QEMU_GA_CONFIG moves the file, for tests.
const guestAgentFilesScript = `want=$1 bool=$2
if command -v getsebool >/dev/null 2>&1 && getsebool "$bool" 2>/dev/null | grep -q -- '--> off$'; then
  setsebool -P "$bool" on 2>/dev/null || true
fi
f=${ACCRETION_QEMU_GA_CONFIG:-` + QemuGAConfig + `}
[ -f "$f" ] || exit 0
awk -v want="$want" '
function fix(list, allow,   n, a, m, w, i, k, keep, out, seen) {
  n = split(list, a, ","); m = split(want, w, ",")
  out = ""
  for (i = 1; i <= n; i++) {
    if (a[i] == "") continue
    keep = 1
    for (k = 1; k <= m; k++) if (a[i] == w[k]) { seen[k] = 1; if (!allow) keep = 0 }
    if (keep) out = out (out == "" ? "" : ",") a[i]
  }
  if (allow) for (k = 1; k <= m; k++) if (!seen[k]) out = out (out == "" ? "" : ",") w[k]
  return out
}
/^[ \t]*#/ { print; next }
match($0, /--allow-rpcs=[^" ]+/) {
  print substr($0, 1, RSTART - 1) "--allow-rpcs=" fix(substr($0, RSTART + 13, RLENGTH - 13), 1) substr($0, RSTART + RLENGTH)
  next
}
match($0, /^[ \t]*(BLACKLIST_RPC|BLOCK_RPCS)=/) {
  key = substr($0, 1, RLENGTH); v = substr($0, RLENGTH + 1); q = ""
  if (v ~ /^".*"$/) { q = "\""; v = substr(v, 2, length(v) - 2) }
  print key q fix(v, 0) q
  next
}
match($0, /--(blacklist|block-rpcs)=[^" $]+/) {
  eq = index(substr($0, RSTART), "=")
  print substr($0, 1, RSTART + eq - 1) fix(substr($0, RSTART + eq, RLENGTH - eq), 0) substr($0, RSTART + RLENGTH)
  next
}
{ print }
' "$f" > "$f.accretion" || { rm -f "$f.accretion"; exit 0; }
if cmp -s "$f" "$f.accretion"; then rm -f "$f.accretion"; exit 0; fi
cat "$f.accretion" > "$f" && rm -f "$f.accretion"
systemctl restart qemu-guest-agent || true
exit 0`

// AllowGuestFileReads is a Base command that lets the guest agent serve file
// reads. Put it after the command that starts the agent and before anything
// the hypervisor is meant to read.
func AllowGuestFileReads() []string {
	return []string{"sh", "-c", guestAgentFilesScript, "guest-agent-files", guestAgentFileRPCs, guestAgentReadBool}
}
