package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

const Infinity = 16

type NetworkConfig struct {
	Routers []string `json:"routers"`
	Links   []Link   `json:"links"`
}

type Link struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Metric int    `json:"metric"`
}

type Route struct {
	SourceIP      string
	DestinationIP string
	NextHop       string
	Metric        int
}

type Neighbor struct {
	IP     string
	Port   int
	Metric int
}

type RouterCfg struct {
	IP            string
	Port          int
	Neighbors     []Neighbor
	Steps         int
	CollectorAddr string
}

type Msg struct {
	Kind   string           `json:"kind"`
	Step   int              `json:"step"`
	FromIP string           `json:"from_ip"`
	Table  map[string]Route `json:"table"`
}

type TmpRouter struct {
	IP        string
	Port      int
	Neighbors map[string]int
}

func main() {
	var (
		cfgPath       = flag.String("config", "", "path to JSON network config")
		rCnt          = flag.Int("routers", 6, "number of routers for random topology")
		seed          = flag.Int64("seed", time.Now().UnixNano(), "random seed")
		steps         = flag.Int("steps", 0, "number of RIP simulation steps; 0 means routers + 2")
		basePort      = flag.Int("base-port", 30000, "first router TCP port")
		collectorPort = flag.Int("collector-port", 29999, "collector TCP port")
	)
	flag.Parse()

	cfgs, err := buildNetwork(*cfgPath, *rCnt, *seed, *basePort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if *steps <= 0 {
		*steps = len(cfgs) + 2
	}

	collectorAddr := fmt.Sprintf("127.0.0.1:%d", *collectorPort)
	for i := range cfgs {
		cfgs[i].Steps = *steps
		cfgs[i].CollectorAddr = collectorAddr
	}

	ln, err := net.Listen("tcp", collectorAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collector error:", err)
		os.Exit(1)
	}
	defer ln.Close()

	fmt.Println("RIP emulator with router threads and TCP sockets started")
	if *cfgPath == "" {
		fmt.Printf("Generated random AS: routers=%d, seed=%d\n", len(cfgs), *seed)
	} else {
		fmt.Printf("Loaded AS config: %s\n", *cfgPath)
	}

	printTopology(cfgs)

	for _, cfg := range cfgs {
		localCfg := copyRouterCfg(cfg)
		go routerProcess(localCfg)
	}

	stepsData, finalData := collectResults(ln, len(cfgs), *steps)

	fmt.Println("\nIntermediate routing tables:")
	printStepTables(stepsData)

	fmt.Println("\nFinal routing tables:")
	printFinalTables(finalData)
}

func buildNetwork(path string, cnt int, seed int64, basePort int) ([]RouterCfg, error) {
	if path != "" {
		cfg, err := readConfig(path)
		if err != nil {
			return nil, err
		}
		return networkFromConfig(cfg, basePort)
	}

	if cnt < 2 {
		return nil, fmt.Errorf("random topology requires at least 2 routers")
	}

	return generateRandomNetwork(cnt, seed, basePort), nil
}

func readConfig(path string) (NetworkConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return NetworkConfig{}, err
	}

	var cfg NetworkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return NetworkConfig{}, err
	}

	return cfg, nil
}

func networkFromConfig(cfg NetworkConfig, basePort int) ([]RouterCfg, error) {
	tmp := make(map[string]*TmpRouter)
	seen := make(map[string]bool)

	for i, ip := range cfg.Routers {
		ip = strings.TrimSpace(ip)

		if ip == "" {
			return nil, fmt.Errorf("router IP cannot be empty")
		}

		if seen[ip] {
			return nil, fmt.Errorf("duplicate router IP: %s", ip)
		}

		seen[ip] = true
		tmp[ip] = &TmpRouter{
			IP:        ip,
			Port:      basePort + i,
			Neighbors: make(map[string]int),
		}
	}

	for _, l := range cfg.Links {
		if l.Metric <= 0 || l.Metric >= Infinity {
			return nil, fmt.Errorf("invalid metric for link %s-%s: metric must be 1..15", l.From, l.To)
		}

		if tmp[l.From] == nil || tmp[l.To] == nil {
			return nil, fmt.Errorf("link references unknown router: %s-%s", l.From, l.To)
		}

		tmp[l.From].Neighbors[l.To] = l.Metric
		tmp[l.To].Neighbors[l.From] = l.Metric
	}

	return makeRouterCfgs(tmp), nil
}

func generateRandomNetwork(n int, seed int64, basePort int) []RouterCfg {
	rng := rand.New(rand.NewSource(seed))
	tmp := make(map[string]*TmpRouter)
	ips := make([]string, 0, n)

	for len(ips) < n {
		ip := randomIP(rng)

		if tmp[ip] != nil {
			continue
		}

		tmp[ip] = &TmpRouter{
			IP:        ip,
			Port:      basePort + len(ips),
			Neighbors: make(map[string]int),
		}

		ips = append(ips, ip)
	}

	for i := 1; i < n; i++ {
		j := rng.Intn(i)
		addLink(tmp, ips[i], ips[j], rng.Intn(4)+1)
	}

	extra := n / 2
	for i := 0; i < extra; i++ {
		a := ips[rng.Intn(n)]
		b := ips[rng.Intn(n)]

		if a == b {
			continue
		}

		if _, ok := tmp[a].Neighbors[b]; ok {
			continue
		}

		addLink(tmp, a, b, rng.Intn(4)+1)
	}

	return makeRouterCfgs(tmp)
}

func addLink(tmp map[string]*TmpRouter, a, b string, m int) {
	tmp[a].Neighbors[b] = m
	tmp[b].Neighbors[a] = m
}

func makeRouterCfgs(tmp map[string]*TmpRouter) []RouterCfg {
	ips := make([]string, 0, len(tmp))
	for ip := range tmp {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	cfgs := make([]RouterCfg, 0, len(tmp))

	for _, ip := range ips {
		r := tmp[ip]

		nIPs := make([]string, 0, len(r.Neighbors))
		for nIP := range r.Neighbors {
			nIPs = append(nIPs, nIP)
		}
		sort.Strings(nIPs)

		ns := make([]Neighbor, 0, len(nIPs))
		for _, nIP := range nIPs {
			ns = append(ns, Neighbor{
				IP:     nIP,
				Port:   tmp[nIP].Port,
				Metric: r.Neighbors[nIP],
			})
		}

		cfgs = append(cfgs, RouterCfg{
			IP:        r.IP,
			Port:      r.Port,
			Neighbors: ns,
		})
	}

	return cfgs
}

func randomIP(rng *rand.Rand) string {
	return fmt.Sprintf(
		"%d.%d.%d.%d",
		rng.Intn(223)+1,
		rng.Intn(256),
		rng.Intn(256),
		rng.Intn(254)+1,
	)
}

func copyRouterCfg(cfg RouterCfg) RouterCfg {
	ns := make([]Neighbor, len(cfg.Neighbors))
	copy(ns, cfg.Neighbors)

	return RouterCfg{
		IP:            cfg.IP,
		Port:          cfg.Port,
		Neighbors:     ns,
		Steps:         cfg.Steps,
		CollectorAddr: cfg.CollectorAddr,
	}
}

func routerProcess(cfg RouterCfg) {
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	defer ln.Close()

	table := initTable(cfg)

	time.Sleep(300 * time.Millisecond)

	sendMsg(cfg.CollectorAddr, Msg{
		Kind:   "step",
		Step:   0,
		FromIP: cfg.IP,
		Table:  copyTable(table),
	})

	for step := 1; step <= cfg.Steps; step++ {
		sendTableToNeighbors(cfg, table)

		deadline := time.Now().Add(450 * time.Millisecond)
		_ = ln.(*net.TCPListener).SetDeadline(deadline)

		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}

			var msg Msg
			err = json.NewDecoder(conn).Decode(&msg)
			_ = conn.Close()

			if err != nil || msg.Kind != "rip" {
				continue
			}

			linkM, ok := linkMetric(cfg, msg.FromIP)
			if !ok {
				continue
			}

			updateTable(table, msg, linkM, cfg.IP)
		}

		sendMsg(cfg.CollectorAddr, Msg{
			Kind:   "step",
			Step:   step,
			FromIP: cfg.IP,
			Table:  copyTable(table),
		})

		time.Sleep(150 * time.Millisecond)
	}

	sendMsg(cfg.CollectorAddr, Msg{
		Kind:   "final",
		Step:   cfg.Steps,
		FromIP: cfg.IP,
		Table:  copyTable(table),
	})
}

func initTable(cfg RouterCfg) map[string]Route {
	table := make(map[string]Route)

	table[cfg.IP] = Route{
		SourceIP:      cfg.IP,
		DestinationIP: cfg.IP,
		NextHop:       "-",
		Metric:        0,
	}

	for _, n := range cfg.Neighbors {
		table[n.IP] = Route{
			SourceIP:      cfg.IP,
			DestinationIP: n.IP,
			NextHop:       n.IP,
			Metric:        n.Metric,
		}
	}

	return table
}

func sendTableToNeighbors(cfg RouterCfg, table map[string]Route) {
	msg := Msg{
		Kind:   "rip",
		FromIP: cfg.IP,
		Table:  copyTable(table),
	}

	for _, n := range cfg.Neighbors {
		addr := fmt.Sprintf("127.0.0.1:%d", n.Port)
		sendMsg(addr, msg)
	}
}

func sendMsg(addr string, msg Msg) {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = json.NewEncoder(conn).Encode(msg)
}

func linkMetric(cfg RouterCfg, ip string) (int, bool) {
	for _, n := range cfg.Neighbors {
		if n.IP == ip {
			return n.Metric, true
		}
	}

	return 0, false
}

func updateTable(table map[string]Route, msg Msg, linkM int, selfIP string) bool {
	changed := false

	for dstIP, r := range msg.Table {
		if dstIP == selfIP {
			continue
		}

		newM := linkM + r.Metric
		if newM > Infinity {
			newM = Infinity
		}

		old, ok := table[dstIP]

		if !ok || newM < old.Metric {
			table[dstIP] = Route{
				SourceIP:      selfIP,
				DestinationIP: dstIP,
				NextHop:       msg.FromIP,
				Metric:        newM,
			}

			changed = true
		}
	}

	return changed
}

func copyTable(src map[string]Route) map[string]Route {
	dst := make(map[string]Route)

	for ip, r := range src {
		dst[ip] = r
	}

	return dst
}

func collectResults(ln net.Listener, routerCount int, steps int) (map[int]map[string]map[string]Route, map[string]map[string]Route) {
	stepsData := make(map[int]map[string]map[string]Route)
	finalData := make(map[string]map[string]Route)
	seenSteps := make(map[string]bool)

	expectedStepMsgs := routerCount * (steps + 1)
	gotStepMsgs := 0

	deadline := time.Now().Add(time.Duration(steps+10) * time.Second)

	for time.Now().Before(deadline) {
		_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(500 * time.Millisecond))

		conn, err := ln.Accept()
		if err != nil {
			if len(finalData) == routerCount && gotStepMsgs >= expectedStepMsgs {
				break
			}
			continue
		}

		var msg Msg
		err = json.NewDecoder(conn).Decode(&msg)
		_ = conn.Close()

		if err != nil {
			continue
		}

		switch msg.Kind {
		case "step":
			key := fmt.Sprintf("%d-%s", msg.Step, msg.FromIP)

			if !seenSteps[key] {
				seenSteps[key] = true
				gotStepMsgs++

				if stepsData[msg.Step] == nil {
					stepsData[msg.Step] = make(map[string]map[string]Route)
				}

				stepsData[msg.Step][msg.FromIP] = msg.Table
			}

		case "final":
			finalData[msg.FromIP] = msg.Table
		}

		if len(finalData) == routerCount && gotStepMsgs >= expectedStepMsgs {
			break
		}
	}

	return stepsData, finalData
}

func printTopology(cfgs []RouterCfg) {
	fmt.Println("\nTopology links:")

	done := make(map[string]bool)

	for _, cfg := range cfgs {
		for _, n := range cfg.Neighbors {
			k1 := cfg.IP + "-" + n.IP
			k2 := n.IP + "-" + cfg.IP

			if done[k1] || done[k2] {
				continue
			}

			done[k1] = true

			fmt.Printf(
				"%s:%d <-> %s:%d, metric=%d\n",
				cfg.IP,
				cfg.Port,
				n.IP,
				n.Port,
				n.Metric,
			)
		}
	}
}

func printStepTables(data map[int]map[string]map[string]Route) {
	steps := make([]int, 0, len(data))
	for step := range data {
		steps = append(steps, step)
	}
	sort.Ints(steps)

	for _, step := range steps {
		ips := make([]string, 0, len(data[step]))
		for ip := range data[step] {
			ips = append(ips, ip)
		}
		sort.Strings(ips)

		for _, ip := range ips {
			fmt.Printf("\nSimulation step %d of router %s\n", step, ip)
			printTable(data[step][ip], ip, false)
		}
	}
}

func printFinalTables(data map[string]map[string]Route) {
	ips := make([]string, 0, len(data))
	for ip := range data {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	for _, ip := range ips {
		fmt.Printf("Final state of router %s table:\n", ip)
		printTable(data[ip], ip, true)
		fmt.Println()
	}
}

func printTable(table map[string]Route, selfIP string, skipSelf bool) {
	fmt.Printf(
		"%-16s %-18s %-16s %s\n",
		"[Source IP]",
		"[Destination IP]",
		"[Next Hop]",
		"[Metric]",
	)

	dsts := make([]string, 0, len(table))

	for dst := range table {
		if skipSelf && dst == selfIP {
			continue
		}

		dsts = append(dsts, dst)
	}

	sort.Strings(dsts)

	for _, dst := range dsts {
		r := table[dst]

		fmt.Printf(
			"%-16s %-18s %-16s %d\n",
			r.SourceIP,
			r.DestinationIP,
			r.NextHop,
			r.Metric,
		)
	}
}
