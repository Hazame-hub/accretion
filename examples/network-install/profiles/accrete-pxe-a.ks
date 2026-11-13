text
cdrom
lang en_US.UTF-8
keyboard us
timezone UTC --utc
network --bootproto=dhcp --hostname=accrete-pxe-a --activate
rootpw --lock
user --name=ops --groups=wheel
sshkey --username=ops "ssh-ed25519 AAAA...replace-with-your-key ops"
ignoredisk --only-use=sda
zerombr
clearpart --all --initlabel --drives=sda
autopart
bootloader --location=mbr --boot-drive=sda
firstboot --disable
selinux --enforcing
services --enabled=sshd,qemu-guest-agent
poweroff

%packages
@^minimal-environment
qemu-guest-agent
%end

%post
echo 'ops ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/ops
chmod 0440 /etc/sudoers.d/ops
%end
