# Generates probe.c with offsetof() for selected fields; values are read from compiled asm.
fields = {
 'sk_buff': 'next prev dev sk tstamp cb _skb_refdst destructor _nfct len data_len mac_len hdr_len queue_mapping priority skb_iif hash vlan_proto vlan_tci napi_id mark inner_transport_header inner_network_header inner_mac_header protocol transport_header network_header mac_header headers_end tail end head data truesize users extensions'.split(),
 'net_device': 'name name_node ifalias mem_end mem_start base_addr irq state dev_list napi_list unreg_list close_list ptype_all ptype_specific adj_list features hw_features wanted_features vlan_features hw_enc_features mpls_features gso_partial_features ifindex group stats rx_dropped tx_dropped rx_nohandler carrier_up_count carrier_down_count wireless_handlers ieee80211_ptr netdev_ops ethtool_ops header_ops flags priv_flags gflags padded operstate link_mode if_port dma mtu min_mtu max_mtu type hard_header_len min_header_len needed_headroom needed_tailroom perm_addr addr_assign_type addr_len upper_level lower_level neigh_priv_len dev_id dev_port addr_list_lock name_assign_type uc_promisc uc mc dev_addrs queues_kset promiscuity allmulti ip_ptr ip6_ptr ieee80211_ptr dev_addr _rx num_rx_queues real_num_rx_queues xdp_prog gro_flush_timeout rx_handler rx_handler_data miniq_ingress ingress_queue broadcast _tx num_tx_queues real_num_tx_queues qdisc tx_queue_len tx_global_lock xps_cpus_map miniq_egress watchdog_timer pcpu_refcnt todo_list link_watch_list priv_destructor nd_net dev sysfs_groups sysfs_rx_queue_group rtnl_link_ops gso_max_size gso_max_segs num_tc tc_to_txq prio_tc_map phydev sfp_bus qdisc_tx_busylock_key qdisc_running_key qdisc_xmit_lock_key addr_list_lock_key proto_down'.split(),
}
fields['module']='state list name init exit core_layout init_layout arch num_exentries'.split()
fields['file']='f_u f_path f_inode f_op f_lock f_flags f_mode f_pos f_owner f_cred private_data f_mapping'.split()
fields['miscdevice']='minor name fops list parent this_device groups nodename mode'.split()
import sys
out = ['#include <linux/skbuff.h>', '#include <linux/netdevice.h>', '#include <net/net_namespace.h>','#include <linux/module.h>','#include <linux/fs.h>','#include <linux/miscdevice.h>','#include <net/sock.h>',
       '#define P(s,f) asm volatile("\\n@@OFF " #s "." #f " %0" :: "i"(offsetof(struct s,f)));',
       '#define SZ(s) asm volatile("\\n@@OFF " #s ".SIZEOF %0" :: "i"(sizeof(struct s)));', 'void probe(void){']
for s, fs in fields.items():
    out.append(f'SZ({s})')
    for f in fs: out.append(f'#if 1\nP({s},{f})\n#endif')
out.append('SZ(net) SZ(file_operations) SZ(net_device_ops) SZ(file) SZ(socket) SZ(sock) SZ(wait_queue_head) SZ(sk_buff_head)}')
open(sys.argv[1], 'w').write('\n'.join(out) + '\n')
