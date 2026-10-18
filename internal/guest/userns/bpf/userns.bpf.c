// SPDX-License-Identifier: GPL-2.0
// Deny creating user namespaces to everyone but real root. Most kernel
// exploits start from an unprivileged user namespace. The decision uses the
// creating credentials: both uid and euid must be 0 (kuids are global, so
// root inside a child namespace does not qualify), which also catches root
// daemons acting with a dropped euid. Loaded by "banken restrict-userns".

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

typedef struct {
	unsigned int val;
} kuid_t;

struct cred {
	kuid_t uid;
	kuid_t euid;
} __attribute__((preserve_access_index));

#define EPERM 1

SEC("lsm/userns_create")
int BPF_PROG(restrict_userns, const struct cred *cred, int ret)
{
	if (ret)
		return ret;
	if (BPF_CORE_READ(cred, uid.val) != 0 || BPF_CORE_READ(cred, euid.val) != 0)
		return -EPERM;
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
