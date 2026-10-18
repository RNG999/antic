// SPDX-License-Identifier: GPL-2.0
// hanare name gate: deny creating names listed in the denied_names map on
// FUSE (virtiofs) filesystems, i.e. the host shares. Loaded by
// hanare-namegate.service (nix/guest.nix).
//
// Covers every way a name appears in a directory: create (incl. open with
// O_CREAT), mkdir, mknod, symlink, link (incl. O_TMPFILE + linkat) and rename
// (security_inode_rename runs this hook for both sides of RENAME_EXCHANGE).

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

// Minimal CO-RE views of kernel types; field offsets are relocated against
// the running kernel's BTF at load time.
struct super_block {
	unsigned long s_magic;
} __attribute__((preserve_access_index));

struct inode {
	struct super_block *i_sb;
} __attribute__((preserve_access_index));

struct qstr {
	const unsigned char *name;
} __attribute__((preserve_access_index));

struct dentry {
	struct qstr d_name;
} __attribute__((preserve_access_index));

typedef unsigned short umode_t;

#define FUSE_SUPER_MAGIC 0x65735546
#define EPERM 1
#define NAME_LEN 64

// Keys are names zero-padded to NAME_LEN bytes.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 128);
	__type(key, char[NAME_LEN]);
	__type(value, __u8);
} denied_names SEC(".maps");

static __always_inline int gate(struct inode *dir, struct dentry *dentry)
{
	char name[NAME_LEN] = {};
	const unsigned char *p;

	if (BPF_CORE_READ(dir, i_sb, s_magic) != FUSE_SUPER_MAGIC)
		return 0;
	p = BPF_CORE_READ(dentry, d_name.name);
	if (bpf_probe_read_kernel_str(name, sizeof(name), p) < 0)
		return 0;
	if (bpf_map_lookup_elem(&denied_names, name))
		return -EPERM;
	return 0;
}

SEC("lsm/inode_create")
int BPF_PROG(gate_create, struct inode *dir, struct dentry *dentry, umode_t mode, int ret)
{
	return ret ? ret : gate(dir, dentry);
}

SEC("lsm/inode_mkdir")
int BPF_PROG(gate_mkdir, struct inode *dir, struct dentry *dentry, umode_t mode, int ret)
{
	return ret ? ret : gate(dir, dentry);
}

SEC("lsm/inode_mknod")
int BPF_PROG(gate_mknod, struct inode *dir, struct dentry *dentry, umode_t mode, unsigned int dev, int ret)
{
	return ret ? ret : gate(dir, dentry);
}

SEC("lsm/inode_symlink")
int BPF_PROG(gate_symlink, struct inode *dir, struct dentry *dentry, const char *old_name, int ret)
{
	return ret ? ret : gate(dir, dentry);
}

SEC("lsm/inode_link")
int BPF_PROG(gate_link, struct dentry *old_dentry, struct inode *dir, struct dentry *new_dentry, int ret)
{
	return ret ? ret : gate(dir, new_dentry);
}

SEC("lsm/inode_rename")
int BPF_PROG(gate_rename, struct inode *old_dir, struct dentry *old_dentry,
	     struct inode *new_dir, struct dentry *new_dentry, int ret)
{
	return ret ? ret : gate(new_dir, new_dentry);
}

char LICENSE[] SEC("license") = "GPL";
