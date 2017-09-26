// Package records contains functions to generate resource records from
// mesos master states to serve through a dns server
package records

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mesosphere/mesos-dns/httpcli"
	"github.com/mesosphere/mesos-dns/logging"
	"github.com/mesosphere/mesos-dns/models"
	"github.com/mesosphere/mesos-dns/records/labels"
	"github.com/mesosphere/mesos-dns/records/state"
	"github.com/mesosphere/mesos-dns/records/state/client"
	"github.com/mesosphere/mesos-dns/urls"
	"github.com/tv42/zbase32"
)

// Map host/service name to DNS answer
// REFACTOR - when discoveryinfo is integrated
// Will likely become map[string][]discoveryinfo
// Effectively we're (ab)using the map type as a set
// It used to have the type: rrs map[string][]string
type rrs map[string]map[string]struct{}

func (r rrs) add(name, host string) bool {
	if host == "" {
		return false
	}
	v, ok := r[name]
	if !ok {
		v = make(map[string]struct{})
		r[name] = v
	} else {
		// don't overwrite existing values
		_, ok = v[host]
		if ok {
			return false
		}
	}
	v[host] = struct{}{}
	return true
}

func (r rrs) First(name string) (string, bool) {
	for host := range r[name] {
		return host, true
	}
	return "", false
}

// Transform the record set into something exportable via the REST API
func (r rrs) ToAXFRResourceRecordSet() models.AXFRResourceRecordSet {
	ret := make(models.AXFRResourceRecordSet, len(r))
	for host, values := range r {
		ret[host] = make([]string, 0, len(values))
		for record := range values {
			ret[host] = append(ret[host], record)
		}
	}
	return ret
}

type rrsKind string

const (
	// A, AAAA record types
	A    rrsKind = "A"
	AAAA rrsKind = "AAAA"
	// SRV record types
	SRV = "SRV"
)

func (kind rrsKind) rrs(rg *RecordGenerator) rrs {
	switch kind {
	case A:
		return rg.As
	case AAAA:
		return rg.AAAAs
	case SRV:
		return rg.SRVs
	default:
		return nil
	}
}

// RecordGenerator contains DNS records and methods to access and manipulate
// them. TODO(kozyraki): Refactor when discovery id is available.
type RecordGenerator struct {
	As          rrs
	AAAAs       rrs
	SRVs        rrs
	SlaveIPs    map[string]string
	EnumData    EnumerationData
	stateLoader func(masters []string) (state.State, error)
}

// EnumerableRecord is the lowest level object, and should map 1:1 with DNS records
type EnumerableRecord struct {
	Name  string `json:"name"`
	Host  string `json:"host"`
	Rtype string `json:"rtype"`
}

// EnumerableTask consists of the records derived from a task
type EnumerableTask struct {
	Name    string             `json:"name"`
	ID      string             `json:"id"`
	Records []EnumerableRecord `json:"records"`
}

// EnumerableFramework is consistent of enumerable tasks, and include the name of the framework
type EnumerableFramework struct {
	Tasks []*EnumerableTask `json:"tasks"`
	Name  string            `json:"name"`
}

// EnumerationData is the top level container pointing to the
// enumerable frameworks containing enumerable tasks
type EnumerationData struct {
	Frameworks []*EnumerableFramework `json:"frameworks"`
}

// Option is a functional configuration type that mutates a RecordGenerator
type Option func(*RecordGenerator)

// WithConfig generates and returns an option that applies some configuration to a RecordGenerator.
// The internal HTTP transport/client is generated upon invocation of this func so that the returned
// Option may be reused by generators that want to share the same transport/client.
func WithConfig(config Config) Option {
	var (
		opt, tlsClientConfig = httpcli.TLSConfig(config.MesosHTTPSOn, config.caPool, config.cert)
		transport            = httpcli.Transport(&http.Transport{
			DisableKeepAlives:   true, // Mesos master doesn't implement defensive HTTP
			MaxIdleConnsPerHost: 2,
			TLSClientConfig:     tlsClientConfig,
		})
		timeout       = httpcli.Timeout(time.Duration(config.StateTimeoutSeconds) * time.Second)
		doer          = httpcli.New(config.MesosAuthentication, config.httpConfigMap, transport, timeout)
		stateEndpoint = urls.Builder{}.With(
			urls.Path("/master/state.json"),
			opt,
		)
	)
	return func(rg *RecordGenerator) {
		rg.stateLoader = client.NewStateLoader(doer, stateEndpoint, func(b []byte, v *state.State) error {
			return json.Unmarshal(b, v)
		})
	}
}

// NewRecordGenerator returns a RecordGenerator that's been configured with a timeout.
func NewRecordGenerator(options ...Option) *RecordGenerator {
	rg := &RecordGenerator{}
	rg.stateLoader = func(_ []string) (s state.State, err error) { return }
	for i := range options {
		if options[i] != nil {
			options[i](rg)
		}
	}
	return rg
}

// ParseState retrieves and parses the Mesos master /state.json and converts it
// into DNS records.
func (rg *RecordGenerator) ParseState(c Config, masters ...string) error {
	// find master -- return if error
	sj, err := rg.stateLoader(masters)
	if err != nil {
		logging.Error.Println("Failed to fetch state.json. Error: ", err)
		return err
	}
	if sj.Leader == "" {
		logging.Error.Println("Unexpected error")
		err = errors.New("empty master")
		return err
	}

	hostSpec := labels.RFC1123
	if c.EnforceRFC952 {
		hostSpec = labels.RFC952
	}

	return rg.InsertState(sj, c.Domain, c.SOAMname, c.Listener, masters, c.IPSources, hostSpec)
}

// hashes a given name using a truncated sha1 hash
// 5 characters extracted from the zbase32 encoded hash provides
// enough entropy to avoid collisions
// zbase32: http://philzimmermann.com/docs/human-oriented-base-32-encoding.txt
// is used to promote human-readable names
func hashString(s string) string {
	hash := sha1.Sum([]byte(s))
	return zbase32.EncodeToString(hash[:])[:5]
}

// attempt to translate the hostname into an IPv4 address. logs an error if IP
// lookup fails. if an IP address cannot be found, returns the same hostname
// that was given. upon success returns the IP address as a string.
func hostToIP4(hostname string) (string, bool) {
	ip := net.ParseIP(hostname)
	if ip == nil {
		t, err := net.ResolveIPAddr("ip4", hostname)
		if err != nil {
			logging.Error.Printf("cannot translate hostname %q into an ip4 address", hostname)
			return hostname, false
		}
		ip = t.IP
	}
	return ip.String(), true
}

// hostToIPs attempts to parse hostname into an ip. If that doesn't work it will perform a
// lookup and try to find all ipv4 and ipv6 ips in the results.
func hostToIPs(hostname string) []net.IP {
	ips := []net.IP{}
	parsedIP := net.ParseIP(hostname)
	if ip := parsedIP.To4(); ip != nil {
		ips = append(ips, ip)
	} else if ip = parsedIP.To16(); ip != nil {
		ips = append(ips, ip)
	} else if addrs, err := net.LookupIP(hostname); err == nil {
		for _, lookupIP := range addrs {
			if ip := lookupIP.To4(); ip != nil {
				ips = append(ips, ip)
			} else if ip := lookupIP.To16(); ip != nil {
				ips = append(ips, ip)
			}
		}
	} else {
		logging.Error.Printf("cannot translate hostname %q into ip addresses: %v", hostname, err)
	}

	return ips
}

// func hostToIPs(hostname string) []net.IP {
// 	ips := []net.IP{}
// 	parsedIP := net.ParseIP(hostname)
// 	if ip := parsedIP.To4(); ip != nil {
// 		ips = append(ips, ip)
// 	} else if ip = parsedIP.To16(); ip != nil {
// 		ips = append(ips, ip)
// 	} else {
// 		if t, err := net.ResolveIPAddr("ip4", hostname); err == nil {
// 			ips = append(ips, t.IP.To4())
// 		}
// 		if t, err := net.ResolveIPAddr("ip6", hostname); err == nil {
// 			ips = append(ips, t.IP.To16())
// 		}
// 	}

// 	if len(ips) == 0 {
// 		logging.Error.Printf("cannot translate hostname %q into ip addresses", hostname)
// 	}

// 	return ips
// }

// InsertState transforms a StateJSON into RecordGenerator RRs
func (rg *RecordGenerator) InsertState(sj state.State, domain, ns, listener string, masters, ipSources []string, spec labels.Func) error {

	rg.SlaveIPs = map[string]string{}
	rg.SRVs = rrs{}
	rg.As = rrs{}
	rg.frameworkRecords(sj, domain, spec)
	rg.slaveRecords(sj, domain, spec)
	rg.listenerRecord(listener, ns)
	rg.masterRecord(domain, masters, sj.Leader)
	rg.taskRecords(sj, domain, spec, ipSources)

	return nil
}

// frameworkRecords injects A and SRV records into the generator store:
//     frameworkname.domain.                 // resolves to IPs of each framework
//     _framework._tcp.frameworkname.domain. // resolves to the driver port and IP of each framework
func (rg *RecordGenerator) frameworkRecords(sj state.State, domain string, spec labels.Func) {
	for _, f := range sj.Frameworks {
		fname := labels.DomainFrag(f.Name, labels.Sep, spec)
		host, port := f.HostPort()
		if addrs := hostToIPs(host); len(addrs) > 0 {
			a := fname + "." + domain + "."
			for _, addr := range addrs {
				if len(addr) == 4 {
					rg.insertRR(a, addr.String(), A)
				} else if len(addr) == 16 {
					rg.insertRR(a, addr.String(), AAAA)
				}
			}
			if port != "" {
				srvAddress := net.JoinHostPort(a, port)
				rg.insertRR("_framework._tcp."+a, srvAddress, SRV)
			}
		}
	}
}

// slaveRecords injects A and SRV records into the generator store:
//     slave.domain.      // resolves to IPs of all slaves
//     _slave._tcp.domain. // resolves to the driver port and IP of all slaves
func (rg *RecordGenerator) slaveRecords(sj state.State, domain string, spec labels.Func) {
	slaveIP := ""
	for _, slave := range sj.Slaves {
		if addrs := hostToIPs(slave.PID.Host); len(addrs) > 0 {
			a := "slave." + domain + "."
			for _, addr := range addrs {
				if len(addr) == 4 {
					if slaveIP == "" {
						slaveIP = addr.String()
					}
					rg.insertRR(a, addr.String(), A)
				} else if len(addr) == 16 {
					rg.insertRR(a, addr.String(), AAAA)
				}
			}
			srv := net.JoinHostPort(a, slave.PID.Port)
			rg.insertRR("_slave._tcp."+domain+".", srv, SRV)
		} else {
			logging.VeryVerbose.Printf("string '%q' for slave with id %q is not a valid IP address", slave.PID.Host, slave.ID)
		}
		if slaveIP == "" {
			slaveIP = labels.DomainFrag(slave.PID.Host, labels.Sep, spec)
		}
		rg.SlaveIPs[slave.ID] = slaveIP
	}
}

// masterRecord injects A and SRV records into the generator store:
//     master.domain.  // resolves to IPs of all masters
//     masterN.domain. // one IP address for each master
//     leader.domain.  // one IP address for the leading master
//
// The current func implementation makes an assumption about the order of masters:
// it's the order in which you expect the enumerated masterN records to be created.
// This is probably important: if a new leader is elected, you may not want it to
// become master0 simply because it's the leader. You probably want your DNS records
// to change as little as possible. And this func should have the least impact on
// enumeration order, or name/IP mappings - it's just creating the records. So let
// the caller do the work of ordering/sorting (if desired) the masters list if a
// different outcome is desired.
//
// Another consequence of the current overall mesos-dns app implementation is that
// the leader may not even be in the masters list at some point in time. masters is
// really fallback-masters (only consider these to be masters if I can't find a
// leader via ZK). At some point in time, they may not actually be masters any more.
// Consider a cluster of 3 nodes that suffers the loss of a member, and gains a new
// member (VM crashed, was replaced by another VM). And the cycle repeats several
// times. You end up with a set of running masters (and leader) that's different
// than the set of statically configured fallback masters.
//
// So the func tries to index the masters as they're listed and begrudgingly assigns
// the leading master an index out-of-band if it's not actually listed in the masters
// list. There are probably better ways to do it.
func (rg *RecordGenerator) masterRecord(domain string, masters []string, leader string) {
	// create records for leader
	// A records
	h := strings.Split(leader, "@")
	if len(h) < 2 {
		logging.Error.Println(leader)
		return // avoid a panic later
	}
	leaderAddress := h[1]
	ip, port, err := urls.SplitHostPort(leaderAddress)

	leaderRecord := "leader." + domain + "."
	allMasterRecord := "master." + domain + "."
	if addrs := hostToIPs(ip); len(addrs) > 0 {
		for _, addr := range addrs {
			if len(addr) == 4 {
				rg.insertRR(leaderRecord, addr.String(), A)
				rg.insertRR(allMasterRecord, addr.String(), A)
			} else if len(addr) == 16 {
				rg.insertRR(leaderRecord, addr.String(), AAAA)
				rg.insertRR(allMasterRecord, addr.String(), AAAA)
			}
		}
	} else {
		rg.insertRR(leaderRecord, ip, A)
		rg.insertRR(allMasterRecord, ip, A)
	}

	if err != nil {
		logging.Error.Println(err)
		return
	}

	// SRV records
	tcp := "_leader._tcp." + domain + "."
	udp := "_leader._udp." + domain + "."
	host := "leader." + domain + "." + ":" + port
	rg.insertRR(tcp, host, SRV)
	rg.insertRR(udp, host, SRV)

	// if there is a list of masters, insert that as well
	addedLeaderMasterN := false
	idx := 0
	for _, master := range masters {
		masterIP, _, err := urls.SplitHostPort(master)
		if err != nil {
			logging.Error.Println(err)
			continue
		}

		// A records (master and masterN)
		if master != leaderAddress {
			added := rg.insertRR(allMasterRecord, masterIP, A)
			if !added {
				// duplicate master?!
				continue
			}
		}

		if master == leaderAddress && addedLeaderMasterN {
			// duplicate leader in masters list?!
			continue
		}

		perMasterRecord := "master" + strconv.Itoa(idx) + "." + domain + "."
		rg.insertRR(perMasterRecord, masterIP, A)
		idx++

		if master == leaderAddress {
			addedLeaderMasterN = true
		}
	}
	// flake: we ended up with a leader that's not in the list of all masters?
	if !addedLeaderMasterN {
		// only a flake if there were fallback masters configured
		if len(masters) > 0 {
			logging.Error.Printf("warning: leader %q is not in master list", leader)
		}
		extraMasterRecord := "master" + strconv.Itoa(idx) + "." + domain + "."
		rg.insertRR(extraMasterRecord, ip, A)
	}
}

// A record for mesos-dns (the name is listed in SOA replies)
func (rg *RecordGenerator) listenerRecord(listener string, ns string) {
	if listener == "0.0.0.0" {
		rg.setFromLocal(listener, ns)
	} else if listener == "127.0.0.1" {
		rg.insertRR(ns, "127.0.0.1", A)
	} else {
		rg.insertRR(ns, listener, A)
	}
}

func (rg *RecordGenerator) taskRecords(sj state.State, domain string, spec labels.Func, ipSources []string) {
	for _, f := range sj.Frameworks {
		enumerableFramework := &EnumerableFramework{
			Name:  f.Name,
			Tasks: []*EnumerableTask{},
		}
		rg.EnumData.Frameworks = append(rg.EnumData.Frameworks, enumerableFramework)

		for _, task := range f.Tasks {
			var ok bool
			task.SlaveIP, ok = rg.SlaveIPs[task.SlaveID]

			// only do running and discoverable tasks
			if ok && (task.State == "TASK_RUNNING") {
				rg.taskRecord(task, f, domain, spec, ipSources, enumerableFramework)
			}
		}
	}
}

type context struct {
	taskName,
	taskID,
	slaveID,
	taskIP,
	slaveIP string
}

func (rg *RecordGenerator) taskRecord(task state.Task, f state.Framework, domain string, spec labels.Func, ipSources []string, enumFW *EnumerableFramework) {

	newTask := &EnumerableTask{ID: task.ID, Name: task.Name}

	enumFW.Tasks = append(enumFW.Tasks, newTask)

	// define context
	ctx := context{
		spec(task.Name),
		hashString(task.ID),
		slaveIDTail(task.SlaveID),
		task.IP(ipSources...),
		task.SlaveIP,
	}

	// use DiscoveryInfo name if defined instead of task name
	if task.HasDiscoveryInfo() {
		// LEGACY TODO: REMOVE
		ctx.taskName = task.DiscoveryInfo.Name
		rg.taskContextRecord(ctx, task, f, domain, spec, newTask)
		// LEGACY, TODO: REMOVE

		ctx.taskName = spec(task.DiscoveryInfo.Name)
		rg.taskContextRecord(ctx, task, f, domain, spec, newTask)
	} else {
		rg.taskContextRecord(ctx, task, f, domain, spec, newTask)
	}

}
func (rg *RecordGenerator) taskContextRecord(ctx context, task state.Task, f state.Framework, domain string, spec labels.Func, enumTask *EnumerableTask) {
	fname := labels.DomainFrag(f.Name, labels.Sep, spec)

	tail := "." + domain + "."

	// insert canonical A records
	canonical := ctx.taskName + "-" + ctx.taskID + "-" + ctx.slaveID + "." + fname
	arec := ctx.taskName + "." + fname

	rg.insertTaskRR(arec+tail, ctx.taskIP, A, enumTask)
	rg.insertTaskRR(canonical+tail, ctx.taskIP, A, enumTask)

	rg.insertTaskRR(arec+".slave"+tail, ctx.slaveIP, A, enumTask)
	rg.insertTaskRR(canonical+".slave"+tail, ctx.slaveIP, A, enumTask)

	// recordName generates records for ctx.taskName, given some generation chain
	recordName := func(gen chain) { gen("_" + ctx.taskName) }

	// asSRV is always the last link in a chain, it must insert RR's
	asSRV := func(target string) chain {
		return func(records ...string) {
			for i := range records {
				name := records[i] + tail
				rg.insertTaskRR(name, target, SRV, enumTask)
			}
		}
	}

	// Add RFC 2782 SRV records
	var subdomains []string
	if task.HasDiscoveryInfo() {
		subdomains = []string{"slave"}
	} else {
		subdomains = []string{"slave", domainNone}
	}

	slaveHost := canonical + ".slave" + tail
	for _, port := range task.Ports() {
		slaveTarget := slaveHost + ":" + port
		recordName(withProtocol(protocolNone, fname, spec,
			withSubdomains(subdomains, asSRV(slaveTarget))))
	}

	if !task.HasDiscoveryInfo() {
		return
	}

	for _, port := range task.DiscoveryInfo.Ports.DiscoveryPorts {
		target := canonical + tail + ":" + strconv.Itoa(port.Number)
		recordName(withProtocol(port.Protocol, fname, spec,
			withNamedPort(port.Name, spec, asSRV(target))))
	}
}

// A records for each local interface
// If this causes problems you should explicitly set the
// listener address in config.json
func (rg *RecordGenerator) setFromLocal(host string, ns string) {

	ifaces, err := net.Interfaces()
	if err != nil {
		logging.Error.Println(err)
	}

	// handle err
	for _, i := range ifaces {

		addrs, err := i.Addrs()
		if err != nil {
			logging.Error.Println(err)
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue
			}

			rg.insertRR(ns, ip.String(), A)
		}
	}
}

// insertRR adds a record to the appropriate record map for the given name/host pair,
// but only if the pair is unique. returns true if added, false otherwise.
// TODO(???): REFACTOR when storage is updated
func (rg *RecordGenerator) insertTaskRR(name, host string, kind rrsKind, enumTask *EnumerableTask) bool {
	if rg.insertRR(name, host, kind) {
		enumRecord := EnumerableRecord{Name: name, Host: host, Rtype: string(kind)}
		enumTask.Records = append(enumTask.Records, enumRecord)
		return true
	}
	return false
}

func (rg *RecordGenerator) insertRR(name, host string, kind rrsKind) (added bool) {
	if rrsByKind := kind.rrs(rg); rrsByKind != nil {
		if added = rrsByKind.add(name, host); added {
			logging.VeryVerbose.Println("[" + string(kind) + "]\t" + name + ": " + host)
		}
	}
	return
}

// return the slave number from a Mesos slave id
func slaveIDTail(slaveID string) string {
	fields := strings.Split(slaveID, "-")
	return strings.ToLower(fields[len(fields)-1])
}
