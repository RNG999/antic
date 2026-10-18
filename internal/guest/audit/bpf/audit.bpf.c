// SPDX-License-Identifier: GPL-2.0
// Activity log for one guest user: program executions (with argv captured
// at exec, before the program can rewrite it), outgoing connections and
// datagrams (inet, inet6, unix, vsock), and files opened for writing.
// Observes only. Events go to user space through a ring buffer; events that
// do not fit are counted in "lost". Loaded by "banken audit".

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

// Minimal CO-RE views of kernel types, relocated against the running
// kernel's BTF at load time.
struct mm_struct {
	unsigned long arg_start;
	unsigned long arg_end;
} __attribute__((preserve_access_index));

struct task_struct {
	int tgid;
	struct task_struct *real_parent;
	struct mm_struct *mm;
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

struct msghdr {
	void *msg_name;
	int msg_namelen;
} __attribute__((preserve_access_index));

struct socket;
struct sockaddr;
typedef int pid_t;

#define FMODE_WRITE 0x2
#define AF_UNIX 1
#define AF_INET 2
#define AF_INET6 10
#define AF_VSOCK 40

enum { EV_EXEC = 1, EV_CONNECT = 2, EV_SEND = 3, EV_WRITE = 4 };
enum { F_TRUNCATED = 1 };

#define ADDR_LEN 112
#define PATH_LEN 256
#define ARGS_LEN 512

// Layout shared with the Go side (event in ../audit.go); every field is
// naturally aligned, so there is no padding.
struct event {
	__u64 cgroup;
	__u32 type;
	__u32 pid;
	__u32 ppid;
	__u32 uid;
	__u32 euid;
	__u16 family;
	__u16 port;      // network byte order (inet, inet6)
	__u32 args_len;
	__u32 flags;
	__u8 addr[ADDR_LEN]; // raw address bytes, family-specific
	char comm[16];
	char path[PATH_LEN];
	char args[ARGS_LEN]; // NUL-separated argv
};

// Set by the loader before load.
volatile const __u32 target_uid = 1000;

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 23);
} events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} lost SEC(".maps");

static __always_inline struct event *start(__u32 type)
{
	__u64 ug = bpf_get_current_uid_gid();
	struct event *e;

	if ((__u32)ug != target_uid)
		return 0;
	e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__u32 k = 0;
		__u64 *n = bpf_map_lookup_elem(&lost, &k);
		if (n)
			(*n)++;
		return 0;
	}
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
	struct mm_struct *mm;
	unsigned long a0, a1, n;

	if (!e)
		return 0;
	e->ppid = BPF_CORE_READ(p, real_parent, tgid);
	if (bpf_probe_read_kernel_str(e->path, sizeof(e->path), BPF_CORE_READ(bprm, filename)) == sizeof(e->path))
		e->flags |= F_TRUNCATED;
	mm = BPF_CORE_READ(p, mm);
	a0 = BPF_CORE_READ(mm, arg_start);
	a1 = BPF_CORE_READ(mm, arg_end);
	n = a1 > a0 ? a1 - a0 : 0;
	if (n > ARGS_LEN) {
		n = ARGS_LEN;
		e->flags |= F_TRUNCATED;
	}
	if (n && bpf_probe_read_user(e->args, n, (void *)a0) == 0)
		e->args_len = n;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// sockaddr reads stay within addrlen; the kernel has copied the address in.
static __always_inline int fill_addr(struct event *e, const void *sa, int len)
{
	__u16 family = 0;

	if (len < 2 || bpf_probe_read_kernel(&family, sizeof(family), sa))
		return -1;
	e->family = family;
	switch (family) {
	case AF_INET:
		if (len < 16)
			return -1;
		bpf_probe_read_kernel(&e->port, 2, (char *)sa + 2);
		bpf_probe_read_kernel(e->addr, 4, (char *)sa + 4);
		return 0;
	case AF_INET6:
		if (len < 24)
			return -1;
		bpf_probe_read_kernel(&e->port, 2, (char *)sa + 2);
		bpf_probe_read_kernel(e->addr, 16, (char *)sa + 8);
		return 0;
	case AF_VSOCK:
		if (len < 12)
			return -1;
		bpf_probe_read_kernel(e->addr, 8, (char *)sa + 4); // port, cid
		return 0;
	case AF_UNIX:
		len -= 2;
		if (len > 108)
			len = 108;
		if (len > 0)
			bpf_probe_read_kernel(e->addr, len, (char *)sa + 2);
		return 0;
	}
	return -1;
}

SEC("lsm/socket_connect")
int BPF_PROG(audit_connect, struct socket *sock, struct sockaddr *address, int addrlen, int ret)
{
	struct event *e = start(EV_CONNECT);

	if (!e)
		return ret;
	if (fill_addr(e, address, addrlen))
		bpf_ringbuf_discard(e, 0);
	else
		bpf_ringbuf_submit(e, 0);
	return ret;
}

// Datagrams and other sends that name a destination (sendto/sendmsg
// without connect, e.g. DNS over UDP).
SEC("lsm/socket_sendmsg")
int BPF_PROG(audit_sendmsg, struct socket *sock, struct msghdr *msg, int size, int ret)
{
	void *name = BPF_CORE_READ(msg, msg_name);
	int len = BPF_CORE_READ(msg, msg_namelen);
	struct event *e;

	if (!name || len <= 0)
		return ret;
	e = start(EV_SEND);
	if (!e)
		return ret;
	if (fill_addr(e, name, len))
		bpf_ringbuf_discard(e, 0);
	else
		bpf_ringbuf_submit(e, 0);
	return ret;
}

SEC("lsm/file_open")
int BPF_PROG(audit_write_open, struct file *file, int ret)
{
	struct event *e;
	long n;

	if (!(BPF_CORE_READ(file, f_mode) & FMODE_WRITE))
		return ret;
	e = start(EV_WRITE);
	if (!e)
		return ret;
	n = bpf_d_path(&file->f_path, e->path, sizeof(e->path));
	if (n < 0)
		e->flags |= F_TRUNCATED;
	bpf_ringbuf_submit(e, 0);
	return ret;
}

char LICENSE[] SEC("license") = "GPL";
