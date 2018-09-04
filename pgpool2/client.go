package pgpool2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreos/go-semver/semver"
)

const (
	// PGPool binary
	PGPool = "pgpool"

	// http://www.pgpool.net/docs/latest/en/html/pcp-commands.html
	PCPNodeCount    = "/usr/sbin/pcp_node_count"
	PCPNodeInfo     = "/usr/sbin/pcp_node_info"
	PCPProcCount    = "/usr/sbin/pcp_proc_count"
	PCPProcInfo     = "/usr/sbin/pcp_proc_info"
	PCPWatchdogInfo = "/usr/sbin/pcp_watchdog_info"

	NodeStatusInitialization = "Initialization"
	NodeStatusUP1            = "Node is up. No connections yet"
	NodeStatusUP2            = "Node is up. Connections are pooled"
	NodeStatusDown           = "Node is down"
	NodeStatusUnknown        = "Unknown node status"

	// do not reorder
	// https://github.com/pgpool/pgpool2/blob/master/src/tools/pcp/pcp_frontend_client.c#L624
	QuorumStateUnknown      = -3
	QuorumStateNoMasterNode = -2
	QuorumStateAbsent       = -1
	QuorumStateOnEdge       = 0
	QuorumStateExist        = 1
)

var (
	PCPValueRegExp      = regexp.MustCompile(`^[^:]+: (.*)$`)
	PGPoolVersionRegExp = regexp.MustCompile(`([0-9]+.[0-9]+.[0-9]+)`)

	PGPoolPCPPassFileSince = semver.New("3.5.0")

	nodeStatusToString = map[int]string{
		0: NodeStatusInitialization,
		1: NodeStatusUP1,
		2: NodeStatusUP2,
		3: NodeStatusDown,
	}

	quorumStateToInt = map[string]int{
		"UNKNOWN":               QuorumStateUnknown,
		"NO MASTER NODE":        QuorumStateNoMasterNode,
		"QUORUM ABSENT":         QuorumStateAbsent,
		"QUORUM IS ON THE EDGE": QuorumStateOnEdge,
		"QUORUM EXIST":          QuorumStateExist,
	}
)

type Options struct {
	PassFile string
	Hostname string
	Port     int
	Username string
	Password string
	Timeout  int
}

type Client struct {
	options            Options
	version            *semver.Version
	pcpPassFileSupport bool
	pcpPassFile        string
	pcpPassFileUser    bool
	pcpPassTempFile    *os.File
}

func NewClient(options Options) (*Client, error) {
	client := &Client{
		options: options,
	}
	ok, err := client.IsSupportPCPPassFile()
	if err != nil {
		return nil, err
	}
	if len(options.PassFile) != 0 {
		client.pcpPassFile = options.PassFile
		client.pcpPassFileUser = true
	}
	if err := client.Validate(); err != nil {
		return nil, err
	}
	if ok {
		client.pcpPassFileSupport = true
		if err := client.createPCPTempFile(); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func (c *Client) IsSupportPCPPassFile() (bool, error) {
	version, err := c.Version()
	if err != nil {
		return false, err
	}
	if version.LessThan(*PGPoolPCPPassFileSince) {
		return false, nil
	}
	return true, nil
}

func (c *Client) Version() (*semver.Version, error) {
	if c.version != nil {
		return c.version, nil
	}
	_, bytesBuffer, err := c.execCommand(PGPool, []string{}, "--version")
	if err != nil {
		return nil, fmt.Errorf("error getting version information: %v", err)
	}
	resultString := strings.TrimSpace(bytesBuffer.String())
	if len(resultString) == 0 {
		return nil, errors.New("pgpool returns empty version information")
	}
	version := c.ExtractVersion(resultString)
	if len(version) == 0 {
		return nil, fmt.Errorf("can't extract pgpool version from string: %s", resultString)
	}
	pgpoolVersion, err := semver.NewVersion(version)
	if err != nil {
		return nil, err
	}
	c.version = pgpoolVersion
	return pgpoolVersion, nil
}

func (c *Client) ExtractVersion(line string) string {
	valueArr := PGPoolVersionRegExp.FindStringSubmatch(line)
	if len(valueArr) > 0 {
		return valueArr[1]
	}
	return ""
}

func (c *Client) createPCPTempFile() error {
	if c.pcpPassFileUser {
		return nil
	}
	f, err := ioutil.TempFile("", "pgpool2")
	if err != nil {
		return err
	}
	_, err = f.WriteString(fmt.Sprintf(
		"%s:%d:%s:%s",
		c.options.Hostname,
		c.options.Port,
		c.options.Username,
		c.options.Password,
	))
	if err != nil {
		return err
	}
	err = f.Chmod(os.FileMode(0600))
	if err != nil {
		return err
	}
	c.pcpPassTempFile = f
	c.pcpPassFile = f.Name()
	return nil
}

func (c *Client) Clean() error {
	if c.pcpPassFileUser {
		return nil
	}
	if c.pcpPassTempFile == nil {
		return nil
	}
	err := os.Remove(c.pcpPassTempFile.Name())
	return err
}

func (c *Client) Validate() error {
	if len(c.options.Hostname) == 0 {
		return errors.New("PCP hostname must be specified")
	}
	if len(c.options.Username) == 0 {
		return errors.New("PCP username must be specified")
	}
	if c.options.Port <= 0 {
		return errors.New("PCP port must be greater than zero")
	}
	if len(c.pcpPassFile) != 0 {
		info, err := os.Stat(c.pcpPassFile)
		if os.IsNotExist(err) {
			return fmt.Errorf("pcppass %s does not exist", c.pcpPassFile)
		}
		if err != nil {
			return fmt.Errorf("cannot retrieve file mode: %v", err)
		}
		if info.IsDir() {
			return fmt.Errorf("pcppass must be a file")
		}
		if info.Mode() != os.FileMode(0600) {
			return fmt.Errorf("unexpected file mode for '%s': %s", c.pcpPassFile, info.Mode().String())
		}
		c.pcpPassFileUser = true
	} else if len(c.options.Password) == 0 {
		return errors.New("PCP password or pcppass file (pgpool-II 3.5 and above) must be specified")
	}
	return nil
}

func (c *Client) execPCPCommand(cmd string, arg ...string) (*bytes.Buffer, error) {
	var (
		argCommon []string
		env       []string
		argResult []string
	)
	if c.pcpPassFileSupport {
		argCommon = []string{
			fmt.Sprintf("--username=%s", c.options.Username),
			fmt.Sprintf("--host=%s", c.options.Hostname),
			fmt.Sprintf("--port=%d", c.options.Port),
			// never prompt for password
			"--no-password",
		}
		env = []string{
			fmt.Sprintf("PCPPASSFILE=%s", c.pcpPassFile),
		}
		argResult = append(argCommon, arg...)
	} else {
		// [global options] timeout hostname port username password [command arguments]
		for _, ar := range arg {
			if strings.HasPrefix(ar, "-") {
				argResult = append(argResult, ar)
			}
		}
		argResult = append(argResult, []string{
			strconv.Itoa(c.options.Timeout),
			c.options.Hostname,
			strconv.Itoa(c.options.Port),
			c.options.Username,
			c.options.Password,
		}...)
		for _, ar := range arg {
			if !strings.HasPrefix(ar, "-") {
				argResult = append(argResult, ar)
			}
		}
	}
	stdOut, _, err := c.execCommand(cmd, env, argResult...)
	if err != nil {
		return stdOut, fmt.Errorf("%v (%s)", err, strings.TrimSpace(stdOut.String()))
	}
	return stdOut, nil
}

func (c *Client) execCommand(cmd string, env []string, arg ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	stdoutBuffer := &bytes.Buffer{}
	stderrBuffer := &bytes.Buffer{}
	pgpoolExec := exec.Command(cmd, arg...)
	pgpoolExec.Env = env
	pgpoolExec.Stdout = stdoutBuffer
	pgpoolExec.Stderr = stderrBuffer
	err := pgpoolExec.Run()
	if err != nil {
		return stdoutBuffer, stderrBuffer, err
	}
	return stdoutBuffer, stderrBuffer, nil
}

func (c *Client) ExecNodeCount() (int, error) {
	bytesBuffer, err := c.execPCPCommand(PCPNodeCount)
	if err != nil {
		return 0, err
	}
	bytes, err := ioutil.ReadAll(bytesBuffer)
	if err != nil {
		return 0, err
	}
	resultString := strings.TrimSpace(string(bytes))
	if len(resultString) == 0 {
		return 0, nil
	}
	resultInt, err := strconv.Atoi(resultString)
	if err != nil {
		return 0, err
	}
	return resultInt, nil
}

type NodeInfo struct {
	Hostname   string
	Port       int
	StatusCode int
	Status     string
	Weight     float64
	Role       string
}

func NodeStatusCodeToString(statusID int) string {
	status, ok := nodeStatusToString[statusID]
	if !ok {
		return NodeStatusUnknown
	}
	return status
}

func ExtractValueFromPCPString(line string) string {
	valueArr := PCPValueRegExp.FindStringSubmatch(line)
	if len(valueArr) > 0 {
		return valueArr[1]
	}
	return ""
}

func NodeInfoUnmarshal(cmdOutBuff io.Reader) (NodeInfo, error) {
	var ni NodeInfo
	reader := bufio.NewReader(cmdOutBuff)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return ni, err
			}
		}
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Hostname") {
			ni.Hostname = ExtractValueFromPCPString(line)
		}
		if strings.Contains(line, "Port") {
			portRaw := ExtractValueFromPCPString(line)
			portInt, err := strconv.Atoi(portRaw)
			if err != nil {
				continue
			}
			ni.Port = portInt
		}
		if strings.Contains(line, "Status") {
			statusRaw := ExtractValueFromPCPString(line)
			statusInt, err := strconv.Atoi(statusRaw)
			if err != nil {
				continue
			}
			ni.StatusCode = statusInt
			ni.Status = NodeStatusCodeToString(statusInt)
		}
		if strings.Contains(line, "Weight") {
			weightRaw := ExtractValueFromPCPString(line)
			weightFloat, err := strconv.ParseFloat(weightRaw, 64)
			if err != nil {
				continue
			}
			ni.Weight = weightFloat
		}
		if strings.Contains(line, "Role") {
			ni.Role = ExtractValueFromPCPString(line)
		}
	}
	return ni, nil
}

func (c *Client) ExecNodeInfo(nodeID int) (NodeInfo, error) {
	var (
		err         error
		bytesBuffer *bytes.Buffer
	)
	if c.pcpPassFileSupport {
		bytesBuffer, err = c.execPCPCommand(PCPNodeInfo, fmt.Sprintf("--node-id=%d", nodeID), "-v")
	} else {
		bytesBuffer, err = c.execPCPCommand(PCPNodeInfo, strconv.Itoa(nodeID), "-v")
	}
	if err != nil {
		return NodeInfo{}, err
	}
	nodeInfo, err := NodeInfoUnmarshal(bytesBuffer)
	if err != nil {
		return NodeInfo{}, err
	}
	return nodeInfo, nil
}

func (c *Client) ExecProcInfo() ([]ProcInfo, error) {
	bytesBuffer, err := c.execPCPCommand(PCPProcInfo, "--all")
	if err != nil {
		return []ProcInfo{}, err
	}
	procInfoArr, err := ProcInfoUnmarshal(bytesBuffer)
	if err != nil {
		return []ProcInfo{}, err
	}
	return procInfoArr, nil
}

type ProcInfoSummary struct {
	Active   map[string]int
	Inactive map[string]int
}

func NewProcInfoSummary() ProcInfoSummary {
	return ProcInfoSummary{
		Active:   make(map[string]int),
		Inactive: make(map[string]int),
	}
}

func (p *ProcInfoSummary) Add(database string, active bool) {
	if active {
		_, ok := p.Active[database]
		if !ok {
			p.Active[database] = 0
		}
		p.Active[database]++
		return
	}
	_, ok := p.Inactive[database]
	if !ok {
		p.Inactive[database] = 0
	}
	p.Inactive[database]++
}

func (c *Client) ProcInfoSummary(pi []ProcInfo) ProcInfoSummary {
	summary := NewProcInfoSummary()
	for _, procInfo := range pi {
		summary.Add(procInfo.Database, procInfo.Connected)
	}
	return summary
}

func (c *Client) ExecProcCount() ([]string, error) {
	bytesBuffer, err := c.execPCPCommand(PCPProcCount)
	if err != nil {
		return []string{}, err
	}
	bytes, err := ioutil.ReadAll(bytesBuffer)
	if err != nil {
		return []string{}, err
	}
	procCountString := strings.TrimSpace(string(bytes))
	procCountArr := strings.Split(procCountString, " ")
	return procCountArr, nil
}

func (c *Client) ExecWatchdogInfo() (WatchdogInfo, error) {
	bytesBuffer, err := c.execPCPCommand(PCPWatchdogInfo, "-v")
	if err != nil {
		return WatchdogInfo{}, err
	}
	watchdogInfo, err := WatchdogInfoUnmarshal(bytesBuffer)
	if err != nil {
		return WatchdogInfo{}, err
	}
	return watchdogInfo, nil
}

type WatchdogInfo struct {
	TotalNodes       int
	RemoteNodes      int
	QuorumState      string
	QuorumStateCode  int
	AliveRemoteNodes int
	VIP              bool
}

func QuorumStateToCode(state string) int {
	if code, ok := quorumStateToInt[state]; ok {
		return code
	}
	return QuorumStateUnknown
}

func WatchdogInfoUnmarshal(cmdOutBuff io.Reader) (WatchdogInfo, error) {
	var wi WatchdogInfo
	reader := bufio.NewReader(cmdOutBuff)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return wi, err
			}
		}
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Total Nodes") {
			totalNodesRaw := ExtractValueFromPCPString(line)
			totalNodesInt, err := strconv.Atoi(totalNodesRaw)
			if err != nil {
				continue
			}
			wi.TotalNodes = totalNodesInt
		}
		if strings.Contains(line, "Remote Nodes") {
			remoteNodesRaw := ExtractValueFromPCPString(line)
			remoteNodesInt, err := strconv.Atoi(remoteNodesRaw)
			if err != nil {
				continue
			}
			wi.RemoteNodes = remoteNodesInt
		}
		if strings.Contains(line, "Quorum state") {
			wi.QuorumState = ExtractValueFromPCPString(line)
			wi.QuorumStateCode = QuorumStateToCode(wi.QuorumState)
		}
		if strings.Contains(line, "Alive Remote Nodes") {
			aliveRemoteNodesRaw := ExtractValueFromPCPString(line)
			aliveRemoteNodesInt, err := strconv.Atoi(aliveRemoteNodesRaw)
			if err != nil {
				continue
			}
			wi.AliveRemoteNodes = aliveRemoteNodesInt
		}
		if strings.Contains(line, "VIP up on local node") {
			vipRaw := ExtractValueFromPCPString(line)
			if vipRaw == "YES" {
				wi.VIP = true
			}
		}
	}
	return wi, nil
}

type ProcInfo struct {
	Database  string
	Username  string
	Connected bool
}

func ProcInfoUnmarshal(cmdOutBuff io.Reader) ([]ProcInfo, error) {
	var pi []ProcInfo
	reader := bufio.NewReader(cmdOutBuff)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return pi, err
			}
		}
		line = strings.TrimSpace(line)
		connectionInfo := strings.Split(line, " ")
		if len(connectionInfo) == 13 {
			procInfo := ProcInfo{
				Database: connectionInfo[0],
				Username: connectionInfo[1],
			}
			if connectionInfo[12] == "1" {
				procInfo.Connected = true
			}
			pi = append(pi, procInfo)
		}
	}
	return pi, nil
}
