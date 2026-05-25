package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
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

type Router struct {
	IP        string
	Neighbors map[string]int
	Table     map[string]Route
}

func main() {
	var (
		cfgPath = flag.String("config", "", "path to JSON network config")
		rCnt = flag.Int("routers", 6, "number of routers for random topology")
		seed = flag.Int64("seed", time.Now().UnixNano(), "random seed")
	)
	flag.Parse()

	rs, err := buildNetwork(*cfgPath, *rCnt, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Println("RIP emulator started")
	if *cfgPath == "" {
		fmt.Printf("Generated random AS: routers=%d, seed=%d\n", len(rs), *seed)
	} else {
		fmt.Printf("Loaded AS config: %s\n", *cfgPath)
	}

	printTopology(rs)

	fmt.Println("\nInitial routing tables:")
	printStepTables(rs, 0)

	iters := runRIP(rs)

	fmt.Printf("\nRIP converged after %d iteration(s).\n\n", iters)
	printFinalTables(rs)
}

func buildNetwork(path string, cnt int, seed int64) (map[string]*Router, error) {
	if path != "" {
		cfg, err := readConfig(path)
		if err != nil {
			return nil, err
		}
		return networkFromConfig(cfg)
	}

	if cnt < 2 {
		return nil, fmt.Errorf("random topology requires at least 2 routers")
	}

	return generateRandomNetwork(cnt, seed), nil
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

func networkFromConfig(cfg NetworkConfig) (map[string]*Router, error) {
	rs := make(map[string]*Router)
	seen := make(map[string]bool)

	for _, ip := range cfg.Routers {
		ip = strings.TrimSpace(ip)

		if ip == "" {
			return nil, fmt.Errorf("router IP cannot be empty")
		}

		if seen[ip] {
			return nil, fmt.Errorf("duplicate router IP: %s", ip)
		}

		seen[ip] = true
		rs[ip] = newRouter(ip)
	}

	for _, l := range cfg.Links {
		if l.Metric <= 0 || l.Metric >= Infinity {
			return nil, fmt.Errorf("invalid metric for link %s-%s: metric must be 1..15", l.From, l.To)
		}

		a, okA := rs[l.From]
		b, okB := rs[l.To]

		if !okA || !okB {
			return nil, fmt.Errorf("link references unknown router: %s-%s", l.From, l.To)
		}

		a.Neighbors[b.IP] = l.Metric
		b.Neighbors[a.IP] = l.Metric
	}

	for _, r := range rs {
		initializeTable(r)
	}

	return rs, nil
}

func generateRandomNetwork(n int, seed int64) map[string]*Router {
	rng := rand.New(rand.NewSource(seed))
	rs := make(map[string]*Router)
	ips := make([]string, 0, n)

	for len(ips) < n {
		ip := randomIP(rng)

		if _, ok := rs[ip]; ok {
			continue
		}

		rs[ip] = newRouter(ip)
		ips = append(ips, ip)
	}

	for i := 1; i < n; i++ {
		j := rng.Intn(i)
		addUndirectedLink(rs, ips[i], ips[j], rng.Intn(4)+1)
	}

	extra := n / 2
	for i := 0; i < extra; i++ {
		a := ips[rng.Intn(n)]
		b := ips[rng.Intn(n)]

		if a == b {
			continue
		}

		if _, ok := rs[a].Neighbors[b]; ok {
			continue
		}

		addUndirectedLink(rs, a, b, rng.Intn(4)+1)
	}

	for _, r := range rs {
		initializeTable(r)
	}

	return rs
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

func newRouter(ip string) *Router {
	return &Router{
		IP:        ip,
		Neighbors: make(map[string]int),
		Table:     make(map[string]Route),
	}
}

func addUndirectedLink(rs map[string]*Router, a, b string, m int) {
	rs[a].Neighbors[b] = m
	rs[b].Neighbors[a] = m
}

func initializeTable(r *Router) {
	r.Table[r.IP] = Route{
		SourceIP:      r.IP,
		DestinationIP: r.IP,
		NextHop:       "-",
		Metric:        0,
	}

	for nIP, m := range r.Neighbors {
		r.Table[nIP] = Route{
			SourceIP:      r.IP,
			DestinationIP: nIP,
			NextHop:       nIP,
			Metric:        m,
		}
	}
}

func runRIP(rs map[string]*Router) int {
	const maxIter = 1000

	for iter := 1; iter <= maxIter; iter++ {
		snaps := snapshotTables(rs)
		changed := false

		for _, curIP := range sortedRouterIPs(rs) {
			cur := rs[curIP]

			for nIP, linkM := range cur.Neighbors {
				nTable := snaps[nIP]

				for dstIP, nRoute := range nTable {
					if dstIP == cur.IP {
						continue
					}

					newM := linkM + nRoute.Metric
					if newM > Infinity {
						newM = Infinity
					}

					old, ok := cur.Table[dstIP]

					if !ok || newM < old.Metric {
						cur.Table[dstIP] = Route{
							SourceIP:      cur.IP,
							DestinationIP: dstIP,
							NextHop:       nIP,
							Metric:        newM,
						}

						changed = true
					}
				}
			}
		}

		if changed {
			printStepTables(rs, iter)
		}

		if !changed {
			return iter - 1
		}
	}

	return maxIter
}

func snapshotTables(rs map[string]*Router) map[string]map[string]Route {
	res := make(map[string]map[string]Route)

	for ip, r := range rs {
		res[ip] = make(map[string]Route)

		for dst, rt := range r.Table {
			res[ip][dst] = rt
		}
	}

	return res
}

func sortedRouterIPs(rs map[string]*Router) []string {
	ips := make([]string, 0, len(rs))

	for ip := range rs {
		ips = append(ips, ip)
	}

	sort.Strings(ips)
	return ips
}

func printTopology(rs map[string]*Router) {
	fmt.Println("\nTopology links:")

	done := make(map[string]bool)

	for _, ip := range sortedRouterIPs(rs) {
		r := rs[ip]

		ns := make([]string, 0, len(r.Neighbors))
		for n := range r.Neighbors {
			ns = append(ns, n)
		}
		sort.Strings(ns)

		for _, n := range ns {
			k1 := ip + "-" + n
			k2 := n + "-" + ip

			if done[k1] || done[k2] {
				continue
			}

			done[k1] = true

			fmt.Printf("%s <-> %s, metric=%d\n", ip, n, r.Neighbors[n])
		}
	}
}

func printStepTables(rs map[string]*Router, step int) {
	for _, ip := range sortedRouterIPs(rs) {
		r := rs[ip]

		fmt.Printf("\nSimulation step %d of router %s\n", step, r.IP)
		printRouterTable(r, false)
	}
}

func printFinalTables(rs map[string]*Router) {
	for _, ip := range sortedRouterIPs(rs) {
		r := rs[ip]

		fmt.Printf("Final state of router %s table:\n", r.IP)
		printRouterTable(r, true)
		fmt.Println()
	}
}

func printRouterTable(r *Router, skipSelf bool) {
	fmt.Printf(
		"%-16s %-18s %-16s %s\n",
		"[Source IP]",
		"[Destination IP]",
		"[Next Hop]",
		"[Metric]",
	)

	dsts := make([]string, 0, len(r.Table))
	for dst := range r.Table {
		if skipSelf && dst == r.IP {
			continue
		}

		dsts = append(dsts, dst)
	}

	sort.Strings(dsts)

	for _, dst := range dsts {
		rt := r.Table[dst]

		fmt.Printf(
			"%-16s %-18s %-16s %d\n",
			rt.SourceIP,
			rt.DestinationIP,
			rt.NextHop,
			rt.Metric,
		)
	}
}
