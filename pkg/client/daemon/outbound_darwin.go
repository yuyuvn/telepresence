package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/telepresence2/v2/pkg/client/daemon/dns"
)

const kubernetesZone = "cluster.local"

type resolveFile struct {
	port        int
	domain      string
	nameservers []net.IP
	search      []string
}

func readResolveFile(fileName string) (*resolveFile, error) {
	fl, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer fl.Close()
	sc := bufio.NewScanner(fl)
	rf := resolveFile{}
	line := 0

	onlyOne := func(key string) error {
		return fmt.Errorf("%q must have a value at %s line %d", key, fileName, line)
	}

	for sc.Scan() {
		line++
		txt := strings.TrimSpace(sc.Text())
		if len(txt) == 0 || strings.HasPrefix(txt, "#") {
			continue
		}
		fields := strings.Fields(txt)
		fc := len(fields)
		if fc == 0 {
			continue
		}
		key := fields[0]
		if fc == 1 {
			return nil, fmt.Errorf("%q must have a value at %s line %d", key, fileName, line)
		}
		value := fields[1]
		switch key {
		case "port":
			if fc != 2 {
				return nil, onlyOne(key)
			}
			rf.port, err = strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid integer at %s line %d", key, fileName, line)
			}
		case "domain":
			if fc != 2 {
				return nil, onlyOne(key)
			}
			rf.domain = value
		case "nameserver":
			if fc != 2 {
				return nil, onlyOne(key)
			}
			ip := net.ParseIP(value)
			if ip == nil {
				return nil, fmt.Errorf("value %q for %q is not a valid IP at %s line %d", value, key, fileName, line)
			}
			rf.nameservers = append(rf.nameservers, ip)
		case "search":
			rf.search = fields[1:]
		default:
			// This reader doesn't do options just yet
			return nil, fmt.Errorf("%q is not a recognized key at %s line %d", key, fileName, line)
		}
	}
	return &rf, nil
}

func (r *resolveFile) write(fileName string) error {
	buf := bytes.NewBufferString("# Generated by telepresence\n")
	fmt.Fprintf(buf, "port %d\n", r.port)
	if r.domain != "" {
		fmt.Fprintf(buf, "domain %s\n", r.domain)
	}
	for _, ns := range r.nameservers {
		fmt.Fprintf(buf, "nameserver %s\n", ns)
	}

	if len(r.search) > 0 {
		buf.WriteString("search")
		for _, s := range r.search {
			buf.WriteByte(' ')
			buf.WriteString(s)
		}
		buf.WriteByte('\n')
	}
	return ioutil.WriteFile(fileName, buf.Bytes(), 0644)
}

func (r *resolveFile) setSearchPaths(paths ...string) {
	ps := make([]string, 0, len(paths)+1)
	for _, p := range paths {
		p = strings.TrimSuffix(p, ".")
		if len(p) > 0 && p != r.domain {
			ps = append(ps, p)
		}
	}
	ps = append(ps, r.domain)
	r.search = ps
}

// dnsServerWorker places a file under the /etc/resolver directory so that it is picked up by the
// MacOS resolver. The file is configured with a single nameserver that points to the local IP
// that the Telepresence DNS server listens to. The file is removed, and the DNS is flushed when
// the worker terminates
//
// For more information about /etc/resolver files, please view the man pages available at
//
//   man 5 resolver
//
// or, if not on a Mac, follow this link: https://www.manpagez.com/man/5/resolver/
func (o *outbound) dnsServerWorker(c context.Context, onReady func()) error {
	resolverDirName := filepath.Join("/etc", "resolver")
	resolverFileName := filepath.Join(resolverDirName, "telepresence.local")

	dnsAddr, err := splitToUDPAddr(o.dnsListener.LocalAddr())
	if err != nil {
		return err
	}

	err = os.MkdirAll(resolverDirName, 0755)
	if err != nil {
		return err
	}

	rf := resolveFile{
		port:        dnsAddr.Port,
		domain:      kubernetesZone,
		nameservers: []net.IP{dnsAddr.IP},
		search:      []string{kubernetesZone},
	}
	if err = rf.write(resolverFileName); err != nil {
		return err
	}
	dlog.Infof(c, "Generated new %s", resolverFileName)

	namespaceResolverFile := func(namespace string) string {
		return filepath.Join(resolverDirName, "telepresence."+namespace+".local")
	}

	o.setSearchPathFunc = func(c context.Context, paths []string) {
		dlog.Infof(c, "setting search paths %s", strings.Join(paths, " "))
		rf, err := readResolveFile(resolverFileName)
		if err != nil {
			dlog.Error(c, err)
			return
		}
		namespaces := make(map[string]struct{})
		search := make([]string, 0)
		for _, path := range paths {
			if strings.ContainsRune(path, '.') {
				search = append(search, path)
			} else if path != "" {
				namespaces[path] = struct{}{}
			}
		}

		// On Darwin, we provide resolution of NAME.NAMESPACE by adding one domain
		// for each namespace in its own domain file under /etc/resolver. Each file
		// is named "telepresence.<domain>.local"
		var removals []string
		var additions []string
		o.domainsLock.Lock()
		for ns := range o.namespaces {
			if _, ok := namespaces[ns]; !ok {
				removals = append(removals, ns)
			}
		}
		for ns := range namespaces {
			if _, ok := o.namespaces[ns]; !ok {
				additions = append(additions, ns)
			}
		}
		o.search = search
		o.namespaces = namespaces
		o.domainsLock.Unlock()

		for _, namespace := range removals {
			nsFile := namespaceResolverFile(namespace)
			dlog.Infof(c, "Removing %s", nsFile)
			if err = os.Remove(nsFile); err != nil {
				dlog.Error(c, err)
			}
		}
		for _, namespace := range additions {
			df := resolveFile{
				port:        dnsAddr.Port,
				domain:      namespace,
				nameservers: []net.IP{dnsAddr.IP},
			}
			nsFile := namespaceResolverFile(namespace)
			dlog.Infof(c, "Generated new %s", nsFile)
			if err = df.write(nsFile); err != nil {
				dlog.Error(c, err)
			}
		}

		rf.setSearchPaths(search...)

		// Versions prior to Big Sur will not trigger an update unless the resolver file
		// is removed and recreated.
		_ = os.Remove(resolverFileName)

		if err = rf.write(resolverFileName); err != nil {
			dlog.Error(c, err)
		}
	}

	defer func() {
		// Remove the main resolver file
		_ = os.Remove(resolverFileName)

		// Remove each namespace resolver file
		for namespace := range o.namespaces {
			_ = os.Remove(namespaceResolverFile(namespace))
		}
		dns.Flush(c)
	}()

	// Start local DNS server
	g := dgroup.NewGroup(c, dgroup.GroupConfig{})
	g.Go("Server", func(c context.Context) error {
		defer o.dnsListener.Close()
		v := dns.NewServer(c, []net.PacketConn{o.dnsListener}, "", o.resolveNoSearch)
		return v.Run(c)
	})
	dns.Flush(c)

	onReady()
	return g.Wait()
}