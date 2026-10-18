// SPDX-License-Identifier: GPL-2.0
// Deny creating user namespaces to everyone but root in the guest. Most
// kernel exploits start from an unprivileged user namespace; root (e.g. the
// nix-daemon build sandbox) keeps working. Loaded by "hanare guest userns".

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define EPERM 1

struct cred;

SEC("lsm/userns_create")
int BPF_PROG(restrict_userns, const struct cred *cred, int ret)
{
	if (ret)
		return ret;
	if ((__u32)bpf_get_current_uid_gid() != 0)
		return -EPERM;
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
