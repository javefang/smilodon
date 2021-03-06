package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
)

type cmdLineOpts struct {
	filters     string
	blockDevice string
	createFs    bool
	fsType      string
	mountFs     bool
	mountPoint  string
	envFile     string
	help        bool
	version     bool
}

var (
	opts              cmdLineOpts
	region            string
	ec2c              *ec2.EC2
	filters           []*ec2.Filter
	volumeAttachTries int
)

func init() {
	flag.StringVar(&opts.filters, "filters", "", "a comma-delimited list of filters. For example --filters='tag-key=Env,tag:Profile=foo'")
	flag.StringVar(&opts.blockDevice, "block-device", "/dev/xvde", "linux block device path")
	flag.BoolVar(&opts.createFs, "create-file-system", false, "whether to create a file system")
	flag.StringVar(&opts.fsType, "file-system-type", "ext4", "file system type")
	flag.BoolVar(&opts.mountFs, "mount-fs", false, "whether to mount a file system")
	flag.StringVar(&opts.mountPoint, "mount-point", "/data", "mount point path")
	flag.StringVar(&opts.envFile, "env-file", "/run/smilodon/environment", "environment file path")
	flag.BoolVar(&opts.help, "help", false, "print this message")
	flag.BoolVar(&opts.version, "version", false, "print version and exit")
}

func main() {
	flag.Parse()

	if flag.NArg() > 0 || opts.help {
		fmt.Fprintf(os.Stderr, "Usage: %q [OPTION]...\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(0)
	}

	if opts.version {
		fmt.Fprintln(os.Stderr, Version)
		os.Exit(0)
	}

	var i instance
	err := i.getMetadata()
	if err != nil {
		log.Fatalf("Issues getting instance metadata properties. Exiting..")
	}
	ec2c = ec2.New(session.New(), aws.NewConfig().WithRegion(i.region))
	disableSourceDestCheck(i.id, ec2c)
	filters = buildFilters(i)

	for {
		run(&i)
		time.Sleep(120 * time.Second)
	}
}

func run(i *instance) {
	// Iterate over found volumes and check if one of them is attached to the
	// instance, then update i.volume accordingly.
	volumes, err := findVolumes(i, ec2c, filters)
	if err != nil {
		log.Println(err)
	} else {
		for _, v := range volumes {
			if i.volume == nil && v.attachedTo == i.id && !v.available {
				log.Printf("Found attached volume: %q.\n", v.id)
				i.volume = &v
				break
			}
			if i.volume != nil && i.volume.id == v.id && v.available {
				i.volume = nil
				break
			}
		}
	}

	// Iterate over found network interfaces and see if one of them is attached
	// to the instance, then update i.networkInterface accordingly.
	networkInterfaces, err := findNetworkInterfaces(i, ec2c, filters)
	if err != nil {
		log.Println(err)
	} else {
		for _, n := range networkInterfaces {
			if i.networkInterface == nil && n.attachedTo == i.id && !n.available {
				log.Printf("Found attached network interface: %q.\n", n.id)
				i.networkInterface = &n
				break
			}
			if i.networkInterface != nil && i.networkInterface.id == n.id && n.available {
				i.networkInterface = nil
				break
			}
		}
	}

	// If nothing is attached, then pick an available volume. We never want to
	// attach a network interface if there is no volume attached first.
	if i.volume == nil && i.networkInterface == nil {
		log.Println("Neither a volume, nor a network interface are attached.")
		for _, v := range volumes {
			if v.available {
				i.attachVolume(v, ec2c)
				break
			}
		}
		if i.volume == nil {
			log.Println("No available volumes found.")
		}
		if i.volume != nil {
			for _, n := range networkInterfaces {
				if n.available && i.volume.nodeID == n.nodeID {
					_ = i.attachNetworkInterface(n, ec2c)
					waitAndSetupIface(n.IPAddress)
					break
				}
				log.Println("No available network interfaces found.")
			}
		} else {
			log.Println("No volumes appear to be attached, skipping network interface attachment.")
		}
	}

	// If volume is attached, but network interface is not, then find a
	// matching available network interface and attach it.
	if i.volume != nil && i.networkInterface == nil {
		for _, n := range networkInterfaces {
			if n.available && n.nodeID == i.volume.nodeID {
				_ = i.attachNetworkInterface(n, ec2c)
				waitAndSetupIface(n.IPAddress)
				break
			}
		}
	}

	// If network interface is attached, but volume is not, then find a
	// matching available volume and attach it. If we cannot find a matching
	// volume after 3 tries, we release the network interface.
	if i.networkInterface != nil && i.volume == nil {
		if volumeAttachTries > 2 {
			log.Println("Unable to attach a matching volume after 3 retries.")
			if err := i.dettachNetworkInterface(); err == nil {
				volumeAttachTries = 0
			}
		}
		for _, v := range volumes {
			if v.available && v.nodeID == i.networkInterface.nodeID {
				log.Printf("Found a matching volume %q with NodeID %q.\n", v.id, v.nodeID)
				if err := i.attachVolume(v, ec2c); err == nil {
					volumeAttachTries = 0
					break
				}
			}
		}
		if i.volume == nil {
			volumeAttachTries++
		}
	}

	// FIXME: below could be cleaned up with less if statements maybe
	// Set node ID. If specified, create and mount the file system.
	if i.volume != nil && i.networkInterface != nil {
		if i.volume.nodeID == i.networkInterface.nodeID {
			if i.nodeID != i.volume.nodeID {
				i.nodeID = i.volume.nodeID
				log.Printf("Node ID is %q.\n", i.nodeID)
				writeEnvFile(opts.envFile, *i)
			}
		}
		// Set nodeID only when both volume and network interface are attached and their node IDs match.
		if i.volume.nodeID != i.networkInterface.nodeID {
			log.Printf("Something has gone wrong, volume and network interface node IDs do not match.")
		}
		if opts.createFs {
			if !hasFs(opts.blockDevice, opts.fsType) {
				mkfs(opts.blockDevice, opts.fsType)
			}
		}
		if opts.mountFs {
			if hasFs(opts.blockDevice, opts.fsType) && !isMounted(opts.blockDevice) {
				mount(opts.blockDevice, opts.mountPoint, opts.fsType)
			}
		}
	}
}

// waitAndSetupIface blocks until network interface becomes ready and gets an
// IP, then set needed sysctl settings.
func waitAndSetupIface(ip string) {
	for tries := 0; tries < 5; tries++ {
		time.Sleep(5 * time.Second)

		iface, err := getIfaceNameByIP(ip)
		if err != nil {
			log.Printf("failed to get interface name: %v", err)
		}
		if iface == "" {
			continue
		}
		if err := setNetRPFilter(iface); err != nil {
			log.Printf("failed to set rp_filter: %v", err)
		} else {
			break
		}
	}
}

// getIfaceNameByIP returns network interface name by IP address.
func getIfaceNameByIP(ip string) (string, error) {
	var name string
	ifaces, err := net.Interfaces()
	if err != nil {
		return name, err
	}
	for _, iface := range ifaces {
		addr, err := iface.Addrs()
		if err != nil {
			return name, err
		}
		for _, a := range addr {
			netIP, _, err := net.ParseCIDR(a.String())
			if err != nil {
				return name, err
			}
			if netIP.Equal(net.ParseIP(ip)) {
				name = iface.Name
			}
		}
	}
	return name, nil
}

// setNetRPFilter sets /proc/sys/net/ipv4/conf/<iface>/rp_filter to value of 2.
// This is needed to accept asymmetrically routed (outgoing routes and incoming
// routes are different) packets on iface interface.
func setNetRPFilter(iface string) error {
	key := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", iface)

	f, err := os.OpenFile(key, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString("2\n"); err != nil {
		return err
	}
	return nil
}
