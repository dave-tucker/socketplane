package ovs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/socketplane/socketplane/Godeps/_workspace/src/github.com/docker/libcontainer/netlink"
	"github.com/socketplane/socketplane/Godeps/_workspace/src/github.com/socketplane/libovsdb"
	"github.com/socketplane/socketplane/ipam"
)

// Gateway addresses are from docker/daemon/networkdriver/bridge/driver.go to reflect similar behaviour
// between this temporary wrapper solution to the native Network integration

var gatewayAddrs = []string{
	// Here we don't follow the convention of using the 1st IP of the range for the gateway.
	// This is to use the same gateway IPs as the /24 ranges, which predate the /16 ranges.
	// In theory this shouldn't matter - in practice there's bound to be a few scripts relying
	// on the internal addressing or other stupid things like that.
	// They shouldn't, but hey, let's not break them unless we really have to.
	"10.1.42.1/16",
	"10.42.42.1/16",
	"172.16.42.1/24",
	"172.16.43.1/24",
	"172.16.44.1/24",
	"10.0.42.1/24",
	"10.0.43.1/24",
	"172.17.42.1/16", // Don't use 172.16.0.0/16, it conflicts with EC2 DNS 172.16.0.23
	"10.0.42.1/16",   // Don't even try using the entire /8, that's too intrusive
	"192.168.42.1/24",
	"192.168.43.1/24",
	"192.168.44.1/24",
}

const mtu = 1514
const defaultBridgeName = "docker0-ovs"

type Bridge struct {
	Name   string
	IP     net.IP
	Subnet *net.IPNet
}

var OvsBridge Bridge = Bridge{Name: defaultBridgeName}

var ovs *libovsdb.OvsdbClient

func init() {
	var err error
	ovs, err = ovs_connect()
	if err != nil {
		log.Println("Error connecting OVS ", err)
	}
}

func getAvailableGwAddress(bridgeIP string) (gwaddr string, err error) {
	if len(bridgeIP) != 0 {
		_, _, err = net.ParseCIDR(bridgeIP)
		if err != nil {
			return
		}
		gwaddr = bridgeIP
	} else {
		for _, addr := range gatewayAddrs {
			_, dockerNetwork, err := net.ParseCIDR(addr)
			if err != nil {
				return "", err
			}
			if err = CheckRouteOverlaps(dockerNetwork); err == nil {
				gwaddr = addr
				return gwaddr, nil
			}
		}
	}
	return "", errors.New("No available GW address")
}

func CreateBridge(bridgeIP string) error {
	ifaceAddr, err := getAvailableGwAddress(bridgeIP)
	if err != nil {
		return fmt.Errorf("Could not find a free IP address range for '%s'", OvsBridge.Name)
	}

	iface, err := net.InterfaceByName(OvsBridge.Name)
	if iface == nil && err != nil {
		if err := createBridgeIface(OvsBridge.Name); err != nil {
			return err
		}
		iface, err = net.InterfaceByName(OvsBridge.Name)
		if err != nil {
			return err
		}
	} else {
		addr, err := GetIfaceAddr(OvsBridge.Name)
		if err != nil {
			return err
		}
		ifaceAddr = addr.String()
	}

	ipAddr, ipNet, err := net.ParseCIDR(ifaceAddr)
	if err != nil {
		return err
	}

	OvsBridge.IP = ipAddr
	OvsBridge.Subnet = ipNet

	if netlink.NetworkLinkAddIp(iface, ipAddr, ipNet); err != nil {
		return fmt.Errorf("Unable to add private network: %s", err)
	}
	if err := netlink.NetworkLinkUp(iface); err != nil {
		return fmt.Errorf("Unable to start network bridge: %s", err)
	}

	err = setupIPTables(OvsBridge.Name, OvsBridge.IP.String())
	if err != nil {
		return err
	}

	return nil
}

func createBridgeIface(name string) error {
	if ovs == nil {
		return errors.New("OVS not connected")
	}
	// TODO : Error handling for CreateOVSBridge.
	CreateOVSBridge(ovs, name)
	// TODO : Lame. Remove the sleep. This is required now to keep netlink happy
	// in the next step to find the created interface.
	time.Sleep(time.Second * 1)
	return nil
}

func AddPeer(peerIp string) error {
	if ovs == nil {
		return errors.New("OVS not connected")
	}
	addVxlanPort(ovs, OvsBridge.Name, "vxlan-"+peerIp, peerIp)
	return nil
}

func DeletePeer(peerIp string) error {
	if ovs == nil {
		return errors.New("OVS not connected")
	}
	deletePort(ovs, OvsBridge.Name, "vxlan-"+peerIp)
	return nil
}

type OvsConnection struct {
	Name    string `json:"name"`
	Ip      string `json:"ip"`
	Subnet  string `json:"subnet"`
	Mac     string `json:"mac"`
	Gateway string `json:"gateway"`
}

func AddConnection(nspid int) (ovsConnection OvsConnection, err error) {
	var (
		bridge = OvsBridge.Name
		prefix = "ovs"
	)
	ovsConnection = OvsConnection{}
	err = nil

	if bridge == "" {
		err = fmt.Errorf("bridge is not available")
		return
	}
	portName, err := createOvsInternalPort(prefix, bridge)
	if err != nil {
		return
	}
	// Add a dummy sleep to make sure the interface is seen by the subsequent calls.
	time.Sleep(time.Second * 1)

	ip := ipam.Request(*OvsBridge.Subnet)
	subnet := OvsBridge.Subnet.String()
	mac := generateMacAddr(ip).String()
	gatewayIp := OvsBridge.IP.String()

	subnetPrefix := subnet[len(subnet)-3 : len(subnet)]

	ovsConnection = OvsConnection{portName, ip.String(), subnetPrefix, mac, gatewayIp}

	if err = SetMtu(portName, mtu); err != nil {
		return
	}
	if err = InterfaceUp(portName); err != nil {
		return
	}
	if err = SetInterfaceInNamespacePid(portName, nspid); err != nil {
		return
	}

	if err = InterfaceDown(portName); err != nil {
		return
	}
	// TODO : Find a way to change the interface name to defaultDevice (eth0).
	// Currently using the Randomly created OVS port as is.
	// refer to veth.go where one end of the veth pair is renamed to eth0
	if err = ChangeInterfaceName(portName, portName); err != nil {
		return
	}

	if err = SetInterfaceIp(portName, ip.String()); err != nil {
		return
	}
	if err = SetInterfaceMac(portName, generateMacAddr(ip).String()); err != nil {
		return
	}

	if err = InterfaceUp(portName); err != nil {
		return
	}
	if err = SetDefaultGateway(OvsBridge.IP.String(), portName); err != nil {
		return
	}
	return ovsConnection, nil
}

func DeleteConnection(portName string) error {
	if ovs == nil {
		return errors.New("OVS not connected")
	}
	deletePort(ovs, OvsBridge.Name, portName)
	return nil
}

// createOvsInternalPort will generate a random name for the
// the port and ensure that it has been created
func createOvsInternalPort(prefix string, bridge string) (port string, err error) {
	if port, err = GenerateRandomName(prefix, 7); err != nil {
		return
	}

	if ovs == nil {
		err = errors.New("OVS not connected")
		return
	}

	AddInternalPort(ovs, bridge, port)
	return
}

// GenerateRandomName returns a new name joined with a prefix.  This size
// specified is used to truncate the randomly generated value
func GenerateRandomName(prefix string, size int) (string, error) {
	id := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(id)[:size], nil
}

func generateMacAddr(ip net.IP) net.HardwareAddr {
	hw := make(net.HardwareAddr, 6)

	// The first byte of the MAC address has to comply with these rules:
	// 1. Unicast: Set the least-significant bit to 0.
	// 2. Address is locally administered: Set the second-least-significant bit (U/L) to 1.
	// 3. As "small" as possible: The veth address has to be "smaller" than the bridge address.
	hw[0] = 0x02

	// The first 24 bits of the MAC represent the Organizationally Unique Identifier (OUI).
	// Since this address is locally administered, we can do whatever we want as long as
	// it doesn't conflict with other addresses.
	hw[1] = 0x42

	// Insert the IP address into the last 32 bits of the MAC address.
	// This is a simple way to guarantee the address will be consistent and unique.
	copy(hw[2:], ip.To4())

	return hw
}

func setupIPTables(bridgeName string, bridgeIP string) error {
	/*
		# Enable IP Masquerade on all ifaces that are not docker-ovs0
		iptables -t nat -A POSTROUTING -s 10.1.42.1 ! -o %bridgeName -j MASQUERADE

		# Enable outgoing connections on all interfaces
		iptables -A FORWARD -i %bridgeName ! -o %bridgeName -j ACCEPT

		# Enable incoming connections for established sessions
		iptables -A FORWARD -o %bridgeName -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
	*/

	log.Println("Setting up iptables")
	natArgs := []string{"-t", "nat", "-A", "POSTROUTING", "-s", bridgeIP, "!", "-o", bridgeName, "-j", "MASQUERADE"}
	output, err := installRule(natArgs...)
	if err != nil {
		return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
	}
	if len(output) != 0 {
		return fmt.Errorf("Error disabling intercontainer communication: %s", output)
	}

	outboundArgs := []string{"-A", "FORWARD", "-i", bridgeName, "!", "-o", bridgeName, "-j", "ACCEPT"}
	output, err = installRule(outboundArgs...)
	if err != nil {
		return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
	}
	if len(output) != 0 {
		return fmt.Errorf("Error disabling intercontainer communication: %s", output)
	}

	inboundArgs := []string{"-A", "FORWARD", "-o", bridgeName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	output, err = installRule(inboundArgs...)
	if err != nil {
		return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
	}
	if len(output) != 0 {
		return fmt.Errorf("Error disabling intercontainer communication: %s", output)
	}
	return nil
}

func installRule(args ...string) ([]byte, error) {
	path, err := exec.LookPath("iptables")
	if err != nil {
		return nil, errors.New("iptables not found")
	}

	output, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("iptables failed: iptables %v: %s (%s)", strings.Join(args, " "), output, err)
	}

	return output, err
}
