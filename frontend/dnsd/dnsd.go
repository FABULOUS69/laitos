package dnsd

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/HouzuoGuo/laitos/env"
	"github.com/HouzuoGuo/laitos/global"
	"github.com/HouzuoGuo/laitos/httpclient"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	RateLimitIntervalSec       = 10   // Rate limit is calculated at 10 seconds interval
	IOTimeoutSec               = 120  // IO timeout for both read and write operations
	MaxPacketSize              = 9038 // Maximum acceptable UDP packet size
	NumQueueRatio              = 10   // Upon initialisation, create (PerIPLimit/NumQueueRatio) number of queues to handle queries.
	BlacklistUpdateIntervalSec = 7200 // Update ad-server blacklist at this interval
	MinNameQuerySize           = 14   // If a query packet is shorter than this length, it cannot possibly be a name query.
	PublicIPRefreshIntervalSec = 1800 // PublicIPRefreshIntervalSec is how often the program places its latest public IP address into array of IPs that may query the server.
	MVPSLicense                = `Disclaimer: this file is free to use for personal use only. Furthermore it is NOT permitted to ` +
		`copy any of the contents or host on any other site without permission or meeting the full criteria of the below license ` +
		` terms. This work is licensed under the Creative Commons Attribution-NonCommercial-ShareAlike License. ` +
		` http://creativecommons.org/licenses/by-nc-sa/4.0/ License info for commercial purposes contact Winhelp2002`
)

// A query to forward to DNS forwarder via DNS.
type UDPQuery struct {
	MyServer    *net.UDPConn
	ClientAddr  *net.UDPAddr
	QueryPacket []byte
}

// A query to forward to DNS forwarder via TCP.
type TCPForwarderQuery struct {
	MyServer    *net.Conn
	QueryPacket []byte
}

// A DNS forwarder daemon that selectively refuse to answer certain A record requests made against advertisement servers.
type DNSD struct {
	Address            string           `json:"Address"`      // Network address for both TCP and UDP to listen to, e.g. 0.0.0.0 for all network interfaces.
	UDPPort            int              `json:"UDPPort"`      // UDP port to listen on
	UDPForwarder       string           `json:"UDPForwarder"` // Forward UDP DNS queries to this address (IP:Port)
	UDPForwarderConns  []net.Conn       `json:"-"`            // UDP connections made toward forwarder
	UDPForwarderQueues []chan *UDPQuery `json:"-"`            // Processing queues that handle UDP forward queries
	UDPBlackHoleQueues []chan *UDPQuery `json:"-"`            // Processing queues that handle UDP black-list answers
	UDPListener        *net.UDPConn     `json:"-"`            // Once UDP daemon is started, this is its listener.

	TCPPort      int          `json:"TCPPort"`      // TCP port to listen on
	TCPForwarder string       `json:"TCPForwarder"` // Forward TCP DNS queries to this address (IP:Port)
	TCPListener  net.Listener `json:"-"`            // Once TCP daemon is started, this is its listener.

	AllowQueryIPPrefixes []string    `json:"AllowQueryIPPrefixes"` // Only allow queries from IP addresses that carry any of the prefixes
	allowQueryMutex      *sync.Mutex `json:"-"`                    // allowQueryMutex guards against concurrent access to AllowQueryIPPrefixes.
	allowQueryLastUpdate int64       `json:"-"`                    // allowQueryLastUpdate is the Unix timestamp of the very latest automatic placement of computer's public IP into the array of AllowQueryIPPrefixes.

	PerIPLimit     int                 `json:"PerIPLimit"` // How many times in 10 seconds interval an IP may send DNS request
	RateLimit      *env.RateLimit      `json:"-"`          // Rate limit counter
	BlackListMutex *sync.Mutex         `json:"-"`          // Protect against concurrent access to black list
	BlackList      map[string]struct{} `json:"-"`          // Do not answer to type A queries made toward these domains
	Logger         global.Logger       `json:"-"`          // Logger
}

// Check configuration and initialise internal states.
func (dnsd *DNSD) Initialise() error {
	dnsd.Logger = global.Logger{ComponentName: "DNSD", ComponentID: fmt.Sprintf("%s:%d&%d", dnsd.Address, dnsd.TCPPort, dnsd.UDPPort)}
	if dnsd.Address == "" {
		return errors.New("DNSD.Initialise: listen address must not be empty")
	}
	if dnsd.UDPPort < 1 && dnsd.TCPPort < 1 {
		return errors.New("DNSD.Initialise: listen port must be greater than 0")
	}
	if dnsd.UDPForwarder == "" && dnsd.TCPForwarder == "" {
		return errors.New("DNSD.Initialise: the server is not useful if UDPForwarder address is empty")
	}
	if dnsd.PerIPLimit < 10 {
		return errors.New("DNSD.Initialise: PerIPLimit must be greater than 9")
	}
	if len(dnsd.AllowQueryIPPrefixes) == 0 {
		return errors.New("DNSD.Initialise: allowable IP prefixes list must not be empty")
	}
	for _, prefix := range dnsd.AllowQueryIPPrefixes {
		if prefix == "" {
			return errors.New("DNSD.Initialise: any allowable IP prefixes must not be empty string")
		}
	}

	dnsd.allowQueryMutex = new(sync.Mutex)
	dnsd.BlackListMutex = new(sync.Mutex)
	dnsd.BlackList = make(map[string]struct{})

	dnsd.RateLimit = &env.RateLimit{
		MaxCount: dnsd.PerIPLimit,
		UnitSecs: RateLimitIntervalSec,
		Logger:   dnsd.Logger,
	}
	dnsd.RateLimit.Initialise()
	// Create a number of forwarder queues to handle incoming UDP DNS queries
	numQueues := dnsd.PerIPLimit / NumQueueRatio
	dnsd.UDPForwarderConns = make([]net.Conn, numQueues)
	dnsd.UDPForwarderQueues = make([]chan *UDPQuery, numQueues)
	dnsd.UDPBlackHoleQueues = make([]chan *UDPQuery, numQueues)
	for i := 0; i < numQueues; i++ {
		forwarderAddr, err := net.ResolveUDPAddr("udp", dnsd.UDPForwarder)
		if err != nil {
			return fmt.Errorf("DNSD.Initialise: failed to resolve address - %v", err)
		}
		forwarderConn, err := net.DialTimeout("udp", forwarderAddr.String(), IOTimeoutSec*time.Second)
		if err != nil {
			return fmt.Errorf("DNSD.Initialise: failed to connect to forwarder - %v", err)
		}
		dnsd.UDPForwarderConns[i] = forwarderConn
		dnsd.UDPForwarderQueues[i] = make(chan *UDPQuery, 16) // there really is no need for a deeper queue
		dnsd.UDPBlackHoleQueues[i] = make(chan *UDPQuery, 4)  // there is also no need for a deeper queue here
	}
	// TCP queries are not handled by queues
	// Always allow server to query itself via public IP
	dnsd.allowMyPublicIP()
	return nil
}

// allowMyPublicIP places the computer's public IP address into the array of IPs allowed to query the server.
func (dnsd *DNSD) allowMyPublicIP() {
	if dnsd.allowQueryLastUpdate+PublicIPRefreshIntervalSec >= time.Now().Unix() {
		return
	}
	dnsd.allowQueryMutex.Lock()
	defer dnsd.allowQueryMutex.Unlock()
	defer func() {
		// This routine runs periodically no matter it succeeded or failed in retrieving latest public IP
		dnsd.allowQueryLastUpdate = time.Now().Unix()
	}()
	latestIP := env.GetPublicIP()
	if latestIP == "" {
		// Not a fatal error if IP cannot be determined
		dnsd.Logger.Warningf("allowMyPublicIP", "", nil, "unable to determine public IP address, the computer will not be able to send query to itself.")
		return
	}
	foundMyIP := false
	for _, allowedIP := range dnsd.AllowQueryIPPrefixes {
		if allowedIP == latestIP {
			foundMyIP = true
			break
		}
	}
	if !foundMyIP {
		// Place latest IP into the array, but do not erase the old IP entries.
		dnsd.AllowQueryIPPrefixes = append(dnsd.AllowQueryIPPrefixes, latestIP)
		dnsd.Logger.Printf("allowMyPublicIP", "", nil, "the latest public IP address %s of this computer is now allowed to query", latestIP)
	}
}

// checkAllowClientIP returns true only if the input IP address is among the allowed addresses.
func (dnsd *DNSD) checkAllowClientIP(clientIP string) bool {
	// At regular time interval, make sure that the latest public IP is allowed to query.
	dnsd.allowMyPublicIP()

	dnsd.allowQueryMutex.Lock()
	defer dnsd.allowQueryMutex.Unlock()
	for _, prefix := range dnsd.AllowQueryIPPrefixes {
		if strings.HasPrefix(clientIP, prefix) {
			return true
		}
	}
	return false
}

// Download ad-servers list from pgl.yoyo.org and return those domain names.
func (dnsd *DNSD) GetAdBlacklistPGL() ([]string, error) {
	yoyo := "https://pgl.yoyo.org/adservers/serverlist.php?hostformat=nohtml&showintro=0&mimetype=plaintext"
	resp, err := httpclient.DoHTTP(httpclient.Request{TimeoutSec: 30}, yoyo)
	if err != nil {
		return nil, err
	}
	if statusErr := resp.Non2xxToError(); statusErr != nil {
		return nil, statusErr
	}
	lines := strings.Split(string(resp.Body), "\n")
	if len(lines) < 100 {
		return nil, fmt.Errorf("DNSD.GetAdBlacklistPGL: PGL's ad-server list is suspiciously short at only %d lines", len(lines))
	}
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		names = append(names, strings.TrimSpace(line))
	}
	return names, nil
}

// Download ad-servers list from winhelp2002.mvps.org and return those domain names.
func (dnsd *DNSD) GetAdBlacklistMVPS() ([]string, error) {
	yoyo := "http://winhelp2002.mvps.org/hosts.txt"
	resp, err := httpclient.DoHTTP(httpclient.Request{TimeoutSec: 30}, yoyo)
	if err != nil {
		return nil, err
	}
	if statusErr := resp.Non2xxToError(); statusErr != nil {
		return nil, statusErr
	}
	// Collect host names from the hosts file content
	names := make([]string, 0, 16384)
	for _, line := range strings.Split(string(resp.Body), "\n") {
		indexZero := strings.Index(line, "0.0.0.0")
		nameEnd := strings.IndexRune(line, '#')
		if indexZero == -1 {
			// Skip lines that do not have a host name
			continue
		}
		if nameEnd == -1 {
			nameEnd = len(line)
		}
		nameBegin := indexZero + len("0.0.0.0")
		if nameBegin >= nameEnd {
			// The line looks like # this is a comment 0.0.0.0

			continue
		}
		names = append(names, strings.TrimSpace(line[nameBegin:nameEnd]))
	}
	if len(names) < 100 {
		return nil, fmt.Errorf("DNSD.GetAdBlacklistMVPS: MVPS' ad-server list is suspiciously short at only %d lines", len(names))
	}
	return names, nil
}

var StandardResponseNoError = []byte{129, 128} // DNS response packet flag - standard response, no indication of error.

//                            Domain     A    IN      TTL 1466  IPv4     0.0.0.0
var BlackHoleAnswer = []byte{192, 12, 0, 1, 0, 1, 0, 0, 5, 186, 0, 4, 0, 0, 0, 0} // DNS answer 0.0.0.0

// Create a DNS response packet without prefix length bytes, that points incoming query to 0.0.0.0.
func RespondWith0(queryNoLength []byte) []byte {
	if queryNoLength == nil || len(queryNoLength) < MinNameQuerySize {
		return []byte{}
	}
	answerPacket := make([]byte, 2+2+len(queryNoLength)-4+len(BlackHoleAnswer))
	// Match transaction ID of original query
	answerPacket[0] = queryNoLength[0]
	answerPacket[1] = queryNoLength[1]
	// 0x8180 - response is a standard query response, without indication of error.
	copy(answerPacket[2:4], StandardResponseNoError)
	// Copy of original query structure
	copy(answerPacket[4:], queryNoLength[4:])
	// There is exactly one answer RR
	answerPacket[6] = 0
	answerPacket[7] = 1
	// Answer 0.0.0.0 to the query
	copy(answerPacket[len(answerPacket)-len(BlackHoleAnswer):], BlackHoleAnswer)
	// Finally, respond!
	return answerPacket
}

/*
Extract domain name asked by the DNS query. Return the domain name itself, and then with leading components removed.
E.g. for a query packet that asks for "a.b.github.com", the function returns:
- a.b.github.com
- b.github.com
- github.com
*/
func ExtractDomainName(packet []byte) (ret []string) {
	ret = make([]string, 0, 8)
	if packet == nil || len(packet) < MinNameQuerySize {
		return
	}
	indexTypeAClassIN := bytes.Index(packet[13:], []byte{0, 1, 0, 1})
	if indexTypeAClassIN < 1 {
		return
	}
	indexTypeAClassIN += 13
	// The byte right before Type-A Class-IN is an empty byte to be discarded
	domainNameBytes := make([]byte, indexTypeAClassIN-13-1)
	copy(domainNameBytes, packet[13:indexTypeAClassIN-1])
	/*
		Restore full-stops of the domain name portion so that it can be checked against black list.
		Not sure why those byte ranges show up in place of full-stops, probably due to some RFCs.
	*/
	for i, b := range domainNameBytes {
		if b <= 44 || b >= 58 && b <= 64 || b >= 91 && b <= 96 {
			domainNameBytes[i] = '.'
		}
	}
	// First return value is domain name unchanged
	domainName := string(domainNameBytes)
	if len(domainName) > 1024 {
		// Domain name is unrealistically long
		return
	}
	ret = append(ret, domainName)
	// Append more of the same domain name, each with leading component removed.
	for {
		index := strings.IndexRune(domainName, '.')
		if index < 1 || index == len(domainName)-1 {
			break
		}
		domainName = domainName[index+1:]
		ret = append(ret, domainName)
	}
	return
}

func (dnsd *DNSD) UpdatedAdBlockLists() {
	pglEntries, pglErr := dnsd.GetAdBlacklistPGL()
	if pglErr == nil {
		dnsd.Logger.Printf("GetAdBlacklistPGL", "", nil, "successfully retrieved ad-blacklist with %d entries", len(pglEntries))
	} else {
		dnsd.Logger.Warningf("GetAdBlacklistPGL", "", pglErr, "failed to update ad-blacklist")
	}
	mvpsEntries, mvpsErr := dnsd.GetAdBlacklistMVPS()
	if mvpsErr == nil {
		dnsd.Logger.Printf("GetAdBlacklistMVPS", "", nil, "successfully retrieved ad-blacklist with %d entries", len(mvpsEntries))
		dnsd.Logger.Printf("GetAdBlacklistMVPS", "", nil, "Please comply with the following liences for your usage of http://winhelp2002.mvps.org/hosts.txt: %s", MVPSLicense)
	} else {
		dnsd.Logger.Warningf("GetAdBlacklistMVPS", "", mvpsErr, "failed to update ad-blacklist")
	}
	dnsd.BlackListMutex.Lock()
	dnsd.BlackList = make(map[string]struct{})
	if pglErr == nil {
		for _, name := range pglEntries {
			dnsd.BlackList[name] = struct{}{}
		}
	}
	if mvpsErr == nil {
		for _, name := range mvpsEntries {
			dnsd.BlackList[name] = struct{}{}
		}
	}
	dnsd.BlackListMutex.Unlock()
	dnsd.Logger.Printf("UpdatedAdBlockLists", "", nil, "ad-blacklist now has %d entries", len(dnsd.BlackList))
}

/*
You may call this function only after having called Initialise()!
Start DNS daemon on configured TCP and UDP ports. Block caller until both listeners are told to stop.
If either TCP or UDP port fails to listen, all listeners are closed and an error is returned.
*/
func (dnsd *DNSD) StartAndBlock() error {
	// Keep updating ad-block black list in background
	stopAdBlockUpdater := make(chan bool, 1)
	go func() {
		dnsd.UpdatedAdBlockLists()
		for {
			select {
			case <-stopAdBlockUpdater:
				return
			case <-time.After(BlacklistUpdateIntervalSec * time.Second):
				dnsd.UpdatedAdBlockLists()
			}
		}
	}()
	numListeners := 0
	errChan := make(chan error, 2)
	if dnsd.UDPPort != 0 {
		numListeners++
		go func() {
			err := dnsd.StartAndBlockUDP()
			errChan <- err
			stopAdBlockUpdater <- true
		}()
	}
	if dnsd.TCPPort != 0 {
		numListeners++
		go func() {
			err := dnsd.StartAndBlockTCP()
			errChan <- err
			stopAdBlockUpdater <- true
		}()
	}
	if numListeners == 0 {
		return fmt.Errorf("DNSD.StartAndBlock: neither UDP nor TCP listen port is defined, the daemon will not start.")
	}
	for i := 0; i < numListeners; i++ {
		if err := <-errChan; err != nil {
			dnsd.Stop()
			return err
		}
	}
	return nil
}

// Close all of open TCP and UDP listeners so that they will cease processing incoming connections.
func (dnsd *DNSD) Stop() {
	if listener := dnsd.TCPListener; listener != nil {
		if err := listener.Close(); err != nil {
			dnsd.Logger.Warningf("Stop", "", err, "failed to close TCP listener")
		}
	}
	if listener := dnsd.UDPListener; listener != nil {
		if err := listener.Close(); err != nil {
			dnsd.Logger.Warningf("Stop", "", err, "failed to close UDP listener")
		}
	}
}

// Return true if any of the input domain names is black listed.
func (dnsd *DNSD) NamesAreBlackListed(names []string) bool {
	dnsd.BlackListMutex.Lock()
	defer dnsd.BlackListMutex.Unlock()
	var blacklisted bool
	for _, name := range names {
		_, blacklisted = dnsd.BlackList[name]
		if blacklisted {
			return true
		}
	}
	return false
}

var githubComTCPQuery, githubComUDPQuery []byte // Sample queries for composing test cases

func init() {
	var err error
	// Prepare two A queries on "github.com" for test cases
	githubComTCPQuery, err = hex.DecodeString("00274cc7012000010000000000010667697468756203636f6d00000100010000291000000000000000")
	if err != nil {
		panic(err)
	}
	githubComUDPQuery, err = hex.DecodeString("e575012000010000000000010667697468756203636f6d00000100010000291000000000000000")
	if err != nil {
		panic(err)
	}
}
