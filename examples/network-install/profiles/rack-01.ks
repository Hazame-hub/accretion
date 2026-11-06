text
cdrom
lang en_US.UTF-8
keyboard us
timezone UTC --utc
network --bootproto=dhcp --hostname=rack-01 --activate
rootpw --lock
user --name=ops --groups=wheel
sshkey --username=ops "ssh-ed25519 AAAA...replace-with-your-key ops"
# Only this disk, by a path that does not change between boots.
ignoredisk --only-use=/dev/disk/by-id/nvme-EXAMPLE_SERIAL
zerombr
clearpart --all --initlabel --drives=/dev/disk/by-id/nvme-EXAMPLE_SERIAL
autopart
bootloader --boot-drive=/dev/disk/by-id/nvme-EXAMPLE_SERIAL
firstboot --disable
selinux --enforcing
services --enabled=sshd
poweroff

%packages
@^minimal-environment
%end

%post
echo 'ops ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/ops
chmod 0440 /etc/sudoers.d/ops
%end
