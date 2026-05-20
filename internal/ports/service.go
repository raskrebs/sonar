package ports

// wellKnownServices maps common listening ports to their IANA-registered
// service names. It is used only as a display hint when the actual process
// behind a port can't be determined (e.g. running unprivileged, where the OS
// hides process info for sockets owned by other users).
var wellKnownServices = map[int]string{
	20:    "ftp-data",
	21:    "ftp",
	22:    "ssh",
	23:    "telnet",
	25:    "smtp",
	53:    "dns",
	67:    "dhcp",
	68:    "dhcp",
	69:    "tftp",
	80:    "http",
	110:   "pop3",
	111:   "rpcbind",
	119:   "nntp",
	123:   "ntp",
	135:   "msrpc",
	139:   "netbios-ssn",
	143:   "imap",
	161:   "snmp",
	179:   "bgp",
	389:   "ldap",
	443:   "https",
	445:   "smb",
	465:   "smtps",
	514:   "syslog",
	515:   "printer",
	587:   "smtp-submission",
	631:   "ipp",
	636:   "ldaps",
	873:   "rsync",
	993:   "imaps",
	995:   "pop3s",
	1080:  "socks",
	1194:  "openvpn",
	1433:  "mssql",
	1521:  "oracle",
	2049:  "nfs",
	2375:  "docker",
	2376:  "docker",
	3306:  "mysql",
	3389:  "rdp",
	5353:  "mdns",
	5432:  "postgresql",
	5672:  "amqp",
	5900:  "vnc",
	6379:  "redis",
	6443:  "kube-apiserver",
	9200:  "elasticsearch",
	11211: "memcached",
	27017: "mongodb",
}

// ServiceName returns the well-known IANA service name for a port, or "" if
// the port is not a recognized well-known port.
func ServiceName(port int) string {
	return wellKnownServices[port]
}
