// SPDX-License-Identifier: GPL-2.0
/*
 * tunlite - minimal /dev/net/tun (layer 3 TUN only) for the Notion 5G (ASR1901) stock kernel.
 *
 * The stock kernel has no CONFIG_TUN and its structures differ from upstream 5.4.288 (vendor fields in
 * sk_buff, net_device, struct module, sock, task_struct...). This driver is built against a tree whose
 * headers were corrected to match the stock modules and kernel image (see ../BUILD.md), and it only touches
 * fields that were verified to match:
 *   - sk_buff: data/len/tail/head/protocol/dev/destructor (no bitfields)
 *   - net_device: fields before 'broadcast' (name, flags, mtu, type, netdev_ops, ...); never netdev_priv(),
 *     the tx queues, pcpu stats or needs_free_netdev
 *   - file: private_data/f_flags; miscdevice; file_operations; net_device_ops
 * It never touches struct sock, inode or task_struct. Per-device state lives in a list owned by this module.
 *
 * Supported ioctls (what OpenVPN and the usual tools use): TUNSETIFF (IFF_TUN, optional IFF_NO_PI),
 * TUNGETIFF, TUNSETPERSIST, TUNGETFEATURES, TUNSETOWNER/TUNSETGROUP (accepted, ignored),
 * TUNSETTXFILTER/TUNSETOFFLOAD with no offloads (accepted). IFF_TAP and multiqueue are rejected.
 */
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/miscdevice.h>
#include <linux/fs.h>
#include <linux/poll.h>
#include <linux/netdevice.h>
#include <linux/skbuff.h>
#include <linux/if_tun.h>
#include <linux/if_arp.h>
#include <linux/if_ether.h>
#include <linux/uaccess.h>
#include <linux/slab.h>
#include <linux/rtnetlink.h>
#include <linux/capability.h>

#define TL_QUEUE_MAX	500
#define TL_MAX_DEVS	16
#define TL_MTU		1500

struct tl_dev {
	struct list_head	list;
	struct net_device	*dev;
	struct sk_buff_head	rxq;		/* kernel -> userspace */
	wait_queue_head_t	wq;
	bool			attached;	/* an open file owns it */
	bool			persist;
	bool			no_pi;
	atomic_long_t		rx_packets, rx_bytes, tx_packets, tx_bytes, tx_dropped, rx_dropped;
};

static LIST_HEAD(tl_list);
static DEFINE_SPINLOCK(tl_list_lock);	/* list membership, used from xmit (softirq) */
static DEFINE_MUTEX(tl_mutex);		/* create/attach/destroy */

static struct tl_dev *tl_find_by_dev(struct net_device *dev)
{
	struct tl_dev *t;

	list_for_each_entry(t, &tl_list, list)
		if (t->dev == dev)
			return t;
	return NULL;
}

/* ---------------------------------------------------------------- net_device side */

static netdev_tx_t tl_xmit(struct sk_buff *skb, struct net_device *dev)
{
	struct tl_dev *t;
	unsigned long flags;

	spin_lock_irqsave(&tl_list_lock, flags);
	t = tl_find_by_dev(dev);
	if (!t || !t->attached || skb_queue_len(&t->rxq) >= TL_QUEUE_MAX) {
		if (t)
			atomic_long_inc(&t->tx_dropped);
		spin_unlock_irqrestore(&tl_list_lock, flags);
		kfree_skb(skb);
		return NETDEV_TX_OK;
	}
	atomic_long_inc(&t->tx_packets);
	atomic_long_add(skb->len, &t->tx_bytes);
	skb_orphan(skb);
	skb_queue_tail(&t->rxq, skb);
	wake_up_interruptible_poll(&t->wq, EPOLLIN | EPOLLRDNORM);
	spin_unlock_irqrestore(&tl_list_lock, flags);
	return NETDEV_TX_OK;
}

static int tl_open(struct net_device *dev) { return 0; }
static int tl_stop(struct net_device *dev) { return 0; }

static void tl_get_stats64(struct net_device *dev, struct rtnl_link_stats64 *s)
{
	struct tl_dev *t;
	unsigned long flags;

	spin_lock_irqsave(&tl_list_lock, flags);
	t = tl_find_by_dev(dev);
	if (t) {
		s->rx_packets = atomic_long_read(&t->rx_packets);
		s->rx_bytes = atomic_long_read(&t->rx_bytes);
		s->tx_packets = atomic_long_read(&t->tx_packets);
		s->tx_bytes = atomic_long_read(&t->tx_bytes);
		s->tx_dropped = atomic_long_read(&t->tx_dropped);
		s->rx_dropped = atomic_long_read(&t->rx_dropped);
	}
	spin_unlock_irqrestore(&tl_list_lock, flags);
}

/*
 * Ops tables are followed by zeroed padding: if the vendor appended members to these structures, the kernel
 * reads NULL there instead of whatever follows in .rodata.
 */
static const struct {
	struct net_device_ops	ops;
	unsigned long		vendor_pad[64];
} tl_netdev_ops = {
	.ops = {
		.ndo_open	 = tl_open,
		.ndo_stop	 = tl_stop,
		.ndo_start_xmit	 = tl_xmit,
		.ndo_get_stats64 = tl_get_stats64,
	},
};

static void tl_setup(struct net_device *dev)
{
	dev->netdev_ops = &tl_netdev_ops.ops;
	dev->type = ARPHRD_NONE;
	dev->hard_header_len = 0;
	dev->addr_len = 0;
	dev->mtu = TL_MTU;
	dev->min_mtu = 68;
	dev->max_mtu = 65535;
	dev->flags = IFF_POINTOPOINT | IFF_NOARP | IFF_MULTICAST;
}

/* ---------------------------------------------------------------- lifecycle */

/* tl_mutex held. Unregisters and frees the device and its state. */
static void tl_destroy(struct tl_dev *t)
{
	unsigned long flags;

	spin_lock_irqsave(&tl_list_lock, flags);
	list_del(&t->list);
	spin_unlock_irqrestore(&tl_list_lock, flags);
	unregister_netdev(t->dev);	/* waits for in-flight xmit */
	free_netdev(t->dev);
	skb_queue_purge(&t->rxq);
	kfree(t);
}

/* tl_mutex held. */
static struct tl_dev *tl_find_by_name(const char *name)
{
	struct tl_dev *t;

	list_for_each_entry(t, &tl_list, list)
		if (!strncmp(t->dev->name, name, IFNAMSIZ))
			return t;
	return NULL;
}

static int tl_set_iff(struct file *file, struct ifreq *ifr)
{
	unsigned short fl = ifr->ifr_flags;
	struct tl_dev *t;
	struct net_device *dev;
	unsigned long flags;
	int n = 0, err;

	if ((fl & TUN_TYPE_MASK) != IFF_TUN)
		return -EINVAL;		/* TAP not supported */
	if (fl & (IFF_MULTI_QUEUE | IFF_ATTACH_QUEUE | IFF_DETACH_QUEUE | IFF_VNET_HDR | IFF_NAPI))
		return -EINVAL;

	mutex_lock(&tl_mutex);
	if (file->private_data) {
		mutex_unlock(&tl_mutex);
		return -EBUSY;
	}
	ifr->ifr_name[IFNAMSIZ - 1] = '\0';
	t = ifr->ifr_name[0] ? tl_find_by_name(ifr->ifr_name) : NULL;
	if (t) {
		if (t->attached) {
			mutex_unlock(&tl_mutex);
			return -EBUSY;
		}
		goto attach;
	}
	if (!capable(CAP_NET_ADMIN)) {
		mutex_unlock(&tl_mutex);
		return -EPERM;
	}
	list_for_each_entry(t, &tl_list, list)
		n++;
	if (n >= TL_MAX_DEVS) {
		mutex_unlock(&tl_mutex);
		return -ENOSPC;
	}

	dev = alloc_netdev(0, ifr->ifr_name[0] ? ifr->ifr_name : "tun%d", NET_NAME_UNKNOWN, tl_setup);
	if (!dev) {
		mutex_unlock(&tl_mutex);
		return -ENOMEM;
	}
	/* __kmalloc, not kzalloc: kzalloc inlines an index into kmalloc_caches[], whose shape depends on the
	 * kernel config; the exported function keeps that decision inside the stock kernel. */
	t = __kmalloc(sizeof(*t), GFP_KERNEL | __GFP_ZERO);
	if (!t) {
		free_netdev(dev);
		mutex_unlock(&tl_mutex);
		return -ENOMEM;
	}
	t->dev = dev;
	skb_queue_head_init(&t->rxq);
	init_waitqueue_head(&t->wq);

	/* Must be on the list before registering: xmit can run as soon as the device is up. */
	spin_lock_irqsave(&tl_list_lock, flags);
	list_add_tail(&t->list, &tl_list);
	spin_unlock_irqrestore(&tl_list_lock, flags);

	err = register_netdev(dev);
	if (err) {
		spin_lock_irqsave(&tl_list_lock, flags);
		list_del(&t->list);
		spin_unlock_irqrestore(&tl_list_lock, flags);
		free_netdev(dev);
		kfree(t);
		mutex_unlock(&tl_mutex);
		return err;
	}

attach:
	t->no_pi = !!(fl & IFF_NO_PI);
	t->attached = true;
	file->private_data = t;
	strscpy(ifr->ifr_name, t->dev->name, IFNAMSIZ);
	ifr->ifr_flags = IFF_TUN | (t->no_pi ? IFF_NO_PI : 0) | (t->persist ? IFF_PERSIST : 0);
	mutex_unlock(&tl_mutex);
	return 0;
}

/* ---------------------------------------------------------------- char device side */

static int tl_chr_open(struct inode *inode, struct file *file)
{
	file->private_data = NULL;	/* misc_open stored the miscdevice here */
	return 0;
}

static int tl_chr_release(struct inode *inode, struct file *file)
{
	struct tl_dev *t;

	mutex_lock(&tl_mutex);
	t = file->private_data;
	if (t) {
		file->private_data = NULL;
		t->attached = false;
		if (!t->persist)
			tl_destroy(t);
		else
			skb_queue_purge(&t->rxq);
	}
	mutex_unlock(&tl_mutex);
	return 0;
}

static ssize_t tl_chr_read(struct file *file, char __user *buf, size_t count, loff_t *pos)
{
	struct tl_dev *t = file->private_data;
	struct sk_buff *skb;
	struct tun_pi pi = { 0, 0 };
	size_t hdr, len;
	int err;

	if (!t)
		return -EBADFD;
	for (;;) {
		skb = skb_dequeue(&t->rxq);
		if (skb)
			break;
		if (file->f_flags & O_NONBLOCK)
			return -EAGAIN;
		err = wait_event_interruptible(t->wq, !skb_queue_empty(&t->rxq) || !t->attached);
		if (err)
			return err;
		if (!t->attached)
			return -EBADFD;
	}
	if (skb_linearize(skb)) {
		kfree_skb(skb);
		return -ENOMEM;
	}
	hdr = t->no_pi ? 0 : sizeof(pi);
	len = skb->len;
	if (count < hdr) {
		kfree_skb(skb);
		return -EINVAL;
	}
	if (hdr) {
		pi.proto = skb->protocol;
		if (len + hdr > count)
			pi.flags = cpu_to_be16(TUN_PKT_STRIP);
		if (copy_to_user(buf, &pi, hdr)) {
			kfree_skb(skb);
			return -EFAULT;
		}
	}
	if (len > count - hdr)
		len = count - hdr;	/* truncated, like tun */
	if (copy_to_user(buf + hdr, skb->data, len)) {
		kfree_skb(skb);
		return -EFAULT;
	}
	consume_skb(skb);
	return hdr + len;
}

static ssize_t tl_chr_write(struct file *file, const char __user *buf, size_t count, loff_t *pos)
{
	struct tl_dev *t = file->private_data;
	struct net_device *dev;
	struct sk_buff *skb;
	struct tun_pi pi = { 0, 0 };
	size_t hdr, len;
	u8 ver;
	__be16 proto;

	if (!t)
		return -EBADFD;
	dev = t->dev;
	hdr = t->no_pi ? 0 : sizeof(pi);
	if (count < hdr + 1 || count > hdr + 65535)
		return -EINVAL;
	len = count - hdr;
	if (hdr && copy_from_user(&pi, buf, hdr))
		return -EFAULT;

	skb = alloc_skb(len + NET_IP_ALIGN, GFP_KERNEL);
	if (!skb) {
		atomic_long_inc(&t->rx_dropped);
		return -ENOMEM;
	}
	skb_reserve(skb, NET_IP_ALIGN);
	if (copy_from_user(skb_put(skb, len), buf + hdr, len)) {
		kfree_skb(skb);
		return -EFAULT;
	}

	if (hdr) {
		proto = pi.proto;
	} else {
		ver = skb->data[0] >> 4;
		if (ver == 4)
			proto = htons(ETH_P_IP);
		else if (ver == 6)
			proto = htons(ETH_P_IPV6);
		else {
			kfree_skb(skb);
			atomic_long_inc(&t->rx_dropped);
			return -EINVAL;
		}
	}
	if (!(dev->flags & IFF_UP)) {
		kfree_skb(skb);
		atomic_long_inc(&t->rx_dropped);
		return -EIO;
	}
	skb->dev = dev;
	skb->protocol = proto;
	skb_reset_mac_header(skb);
	skb_reset_network_header(skb);

	atomic_long_inc(&t->rx_packets);
	atomic_long_add(len, &t->rx_bytes);
	netif_rx_ni(skb);
	return count;
}

static __poll_t tl_chr_poll(struct file *file, poll_table *wait)
{
	struct tl_dev *t = file->private_data;
	__poll_t mask = EPOLLOUT | EPOLLWRNORM;

	if (!t)
		return EPOLLERR;
	poll_wait(file, &t->wq, wait);
	if (!skb_queue_empty(&t->rxq))
		mask |= EPOLLIN | EPOLLRDNORM;
	return mask;
}

static long tl_chr_ioctl(struct file *file, unsigned int cmd, unsigned long arg)
{
	void __user *argp = (void __user *)arg;
	struct tl_dev *t;
	struct ifreq ifr;
	int ret;

	switch (cmd) {
	case TUNGETFEATURES:
		return put_user(IFF_TUN | IFF_NO_PI, (unsigned int __user *)argp);
	case TUNSETIFF:
		if (copy_from_user(&ifr, argp, sizeof(ifr)))
			return -EFAULT;
		ret = tl_set_iff(file, &ifr);
		if (!ret && copy_to_user(argp, &ifr, sizeof(ifr)))
			return -EFAULT;
		return ret;
	}

	mutex_lock(&tl_mutex);
	t = file->private_data;
	if (!t) {
		mutex_unlock(&tl_mutex);
		return -EBADFD;
	}
	ret = 0;
	switch (cmd) {
	case TUNGETIFF:
		memset(&ifr, 0, sizeof(ifr));
		strscpy(ifr.ifr_name, t->dev->name, IFNAMSIZ);
		ifr.ifr_flags = IFF_TUN | (t->no_pi ? IFF_NO_PI : 0) | (t->persist ? IFF_PERSIST : 0);
		if (copy_to_user(argp, &ifr, sizeof(ifr)))
			ret = -EFAULT;
		break;
	case TUNSETPERSIST:
		t->persist = !!arg;
		break;
	case TUNSETOWNER:
	case TUNSETGROUP:
	case TUNSETTXFILTER:
		break;
	case TUNSETOFFLOAD:
		ret = arg ? -EINVAL : 0;
		break;
	default:
		ret = -EINVAL;
	}
	mutex_unlock(&tl_mutex);
	return ret;
}

static const struct {
	struct file_operations	fops;
	unsigned long		vendor_pad[32];
} tl_fops = {
	.fops = {
		.owner		= THIS_MODULE,
		.llseek		= no_llseek,
		.read		= tl_chr_read,
		.write		= tl_chr_write,
		.poll		= tl_chr_poll,
		.unlocked_ioctl	= tl_chr_ioctl,
		.open		= tl_chr_open,
		.release	= tl_chr_release,
	},
};

static struct {
	struct miscdevice	misc;
	unsigned long		vendor_pad[16];
} tl_misc = {
	.misc = {
		.minor		= TUN_MINOR,
		.name		= "tun",
		.nodename	= "net/tun",
		.fops		= &tl_fops.fops,
	},
};

static int __init tl_init(void)
{
	int err = misc_register(&tl_misc.misc);

	if (err)
		pr_err("tunlite: misc_register failed: %d\n", err);
	else
		pr_info("tunlite: /dev/net/tun ready (layer 3 only)\n");
	return err;
}

static void __exit tl_exit(void)
{
	struct tl_dev *t, *n;

	misc_deregister(&tl_misc.misc);
	mutex_lock(&tl_mutex);
	list_for_each_entry_safe(t, n, &tl_list, list)
		tl_destroy(t);
	mutex_unlock(&tl_mutex);
}

module_init(tl_init);
module_exit(tl_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("Minimal layer-3 TUN device for the Notion 5G stock kernel");
MODULE_ALIAS_MISCDEV(TUN_MINOR);
MODULE_ALIAS("devname:net/tun");
