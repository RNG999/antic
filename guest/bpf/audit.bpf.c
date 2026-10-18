// SPDX-License-Identifier: GPL-2.0
// Activity log for one guest user: program executions, outgoing connections
// and files opened for writing, sent to user space through a ring buffer.
// Observes only (never denies). Loaded by "hanare guest audit".

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

// Minimal CO-RE views of kernel types, relocated against the running
// kernel's BTF at load time.
struct task_struct {
	int tgid;
	struct task_struct *real_parent;
} __attribute__((preserve_access_index));

struct linux_binprm {
	const char *filename;
} __attribute__((preserve_access_index));

struct path {
	void *mnt;
	void *dentry;
} __attribute__((preserve_access_index));

struct file {
	struct path f_path;
	unsigned int f_mode;
} __attribute__((preserve_access_index));

struct socket;
struct sockaddr;
typedef int pid_t;

#define FMODE_WRITE 0x2
#define AF_INET 2
#define AF_INET6 10

enum { EV_EXEC = 1, EV_CONNECT = 2, EV_WRITE = 3 };

// Layout shared with the Go side (event in ../audit.go); no implicit padding.
struct event {
	__u64 cgroup;
	__u32 type;
	__u32 pid;
	__u32 ppid;
	__u32 uid;
	__u16 family;
	__u16 port;
	__u8 addr[16];
	char comm[16];
	char path[256];
	__u32 pad;
};

// Set by the loader before load.
volatile const __u32 target_uid = 1000;

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 22);
} events SEC(".maps");

static __always_inline struct event *start(__u32 type)
{
	__u64 ug = bpf_get_current_uid_gid();
	struct event *e;

	if ((__u32)ug != target_uid)
		return 0;
	e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;
	__builtin_memset(e, 0, sizeof(*e));
	e->type = type;
	e->uid = (__u32)ug;
	e->pid = bpf_get_current_pid_tgid() >> 32;
	e->cgroup = bpf_get_current_cgroup_id();
	bpf_get_current_comm(e->comm, sizeof(e->comm));
	return e;
}

SEC("tp_btf/sched_process_exec")
int BPF_PROG(audit_exec, struct task_struct *p, pid_t old_pid, struct linux_binprm *bprm)
{
	struct event *e = start(EV_EXEC);

	if (!e)
		return 0;
	e->ppid = BPF_CORE_READ(p, real_parent, tgid);
	bpf_probe_read_kernel_str(e->path, sizeof(e->path), BPF_CORE_READ(bprm, filename));
	bpf_ringbuf_submit(e, 0);
	return 0;
}

SEC("lsm/socket_connect")
int BPF_PROG(audit_connect, struct socket *sock, struct sockaddr *address, int addrlen, int ret)
{
	__u16 family = 0;
	struct event *e;

	bpf_probe_read_kernel(&family, sizeof(family), address);
	if (family != AF_INET && family != AF_INET6)
		return ret;
	e = start(EV_CONNECT);
	if (!e)
		return ret;
	e->family = family;
	bpf_probe_read_kernel(&e->port, sizeof(e->port), (char *)address + 2);
	if (family == AF_INET)
		bpf_probe_read_kernel(e->addr, 4, (char *)address + 4);
	else
		bpf_probe_read_kernel(e->addr, 16, (char *)address + 8);
	bpf_ringbuf_submit(e, 0);
	return ret;
}

SEC("lsm/file_open")
int BPF_PROG(audit_write_open, struct file *file, int ret)
{
	struct event *e;

	if (!(BPF_CORE_READ(file, f_mode) & FMODE_WRITE))
		return ret;
	e = start(EV_WRITE);
	if (!e)
		return ret;
	bpf_d_path(&file->f_path, e->path, sizeof(e->path));
	bpf_ringbuf_submit(e, 0);
	return ret;
}

char LICENSE[] SEC("license") = "GPL";
