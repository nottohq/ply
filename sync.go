package main

// Sharing between repositories. Because a state is a set that only grows,
// syncing is "send what the other side lacks, then take the union". There is
// no such thing as a rejected push: two sides that both moved simply combine.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Peer interface {
	Ref(name string) (string, error)                  // root of a workspace there ("" if it does not exist)
	Node(hash string) ([]byte, error)                 // one node of a state trie
	Nodes(hashes []string) (map[string][]byte, error) // several nodes at once
	Lacks(hashes []string) ([]string, error)          // which of these nodes it does not have
	PutNodes(nodes map[string][]byte) error           // store nodes
	Changes(ids []string) ([]*Wire, error)            // changes by id
	Push(name string, cs []*Wire) (string, error)     // store changes and add them to a workspace
	Close()
}

// ===== a repository on this machine =====

type localPeer struct{ r *Repo }

func (p localPeer) Ref(name string) (string, error) {
	w, err := p.r.loadWS(name)
	if err != nil {
		return "", nil
	}
	return w.Root, nil
}

func (p localPeer) Node(hash string) ([]byte, error) {
	b := p.r.rawNode(hash)
	if b == nil {
		return nil, fmt.Errorf("no such node %s", hash)
	}
	return b, nil
}

func (p localPeer) Nodes(hashes []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, h := range hashes {
		b := p.r.rawNode(h)
		if b == nil {
			return nil, fmt.Errorf("no such node %s", h)
		}
		out[h] = b
	}
	return out, nil
}

func (p localPeer) Lacks(hashes []string) ([]string, error) {
	out := []string{}
	for _, h := range hashes {
		if !p.r.hasNode(h) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (p localPeer) PutNodes(nodes map[string][]byte) error {
	for h, b := range nodes {
		if nodeHash(b) != h {
			return fmt.Errorf("node %s does not match its name", h)
		}
		p.r.putRawNode(h, b)
	}
	return nil
}

func (p localPeer) Changes(ids []string) ([]*Wire, error) {
	out := make([]*Wire, 0, len(ids))
	for _, id := range ids {
		w := p.r.wire(id)
		if w == nil {
			return nil, fmt.Errorf("no such change %s", id)
		}
		out = append(out, w)
	}
	return out, nil
}

func (p localPeer) Push(name string, cs []*Wire) (string, error) {
	w, err := p.r.loadWS(name)
	if err != nil {
		w = &Workspace{Name: name, Bare: true}
	}
	for _, c := range cs {
		m, err := p.r.storeWire(c)
		if err != nil {
			return "", err
		}
		w.Root = trieInsert(p.r, w.Root, m.entry())
	}
	// The files of a workspace someone is working in are left alone; its next
	// command records their edits first and then brings the files up to date.
	return w.Root, p.r.saveWS(w)
}

func (p localPeer) Close() {
	p.r.commit()
	p.r.close()
}

// ===== a repository over HTTP =====

type httpPeer struct {
	base  string
	token string
	c     *http.Client
}

func (p *httpPeer) do(method, path string, body, into any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.base+path, rd)
	if err != nil {
		return 0, err
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	res, err := p.c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode >= 400 {
		return res.StatusCode, fmt.Errorf("remote said %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	if raw, ok := into.(*[]byte); ok {
		*raw = b
		return res.StatusCode, nil
	}
	return res.StatusCode, json.Unmarshal(b, into)
}

func (p *httpPeer) Ref(name string) (string, error) {
	var out struct{ Root string }
	code, err := p.do("GET", "/ref/"+url.PathEscape(name), nil, &out)
	if code == 404 {
		return "", nil
	}
	return out.Root, err
}

func (p *httpPeer) Node(hash string) ([]byte, error) {
	var b []byte
	_, err := p.do("GET", "/node/"+hash, nil, &b)
	return b, err
}

func (p *httpPeer) Nodes(hashes []string) (map[string][]byte, error) {
	var out struct{ Nodes map[string][]byte }
	_, err := p.do("POST", "/nodes", map[string]any{"hashes": hashes}, &out)
	return out.Nodes, err
}

func (p *httpPeer) Lacks(hashes []string) ([]string, error) {
	var out struct{ Lacks []string }
	_, err := p.do("POST", "/lacks", map[string]any{"hashes": hashes}, &out)
	return out.Lacks, err
}

func (p *httpPeer) PutNodes(nodes map[string][]byte) error {
	var out struct{}
	_, err := p.do("POST", "/putnodes", map[string]any{"nodes": nodes}, &out)
	return err
}

func (p *httpPeer) Changes(ids []string) ([]*Wire, error) {
	var out struct{ Changes []*Wire }
	_, err := p.do("POST", "/changes", map[string]any{"ids": ids}, &out)
	return out.Changes, err
}

func (p *httpPeer) Push(name string, cs []*Wire) (string, error) {
	var out struct{ Root string }
	_, err := p.do("POST", "/push/"+url.PathEscape(name), map[string]any{"changes": cs}, &out)
	return out.Root, err
}

func (p *httpPeer) Close() {}

func openPeer(target string) (Peer, error) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return &httpPeer{base: strings.TrimRight(target, "/"), token: os.Getenv("PLY_TOKEN"), c: &http.Client{Timeout: 5 * time.Minute}}, nil
	}
	root, err := findRoot(target)
	if err != nil {
		return nil, fmt.Errorf("%s is not a ply repository", target)
	}
	if abs, _ := filepath.Abs(target); filepath.Clean(abs) != filepath.Clean(root) {
		return nil, fmt.Errorf("%s is inside a repository but is not its top directory", target)
	}
	r, err := openRepo(root)
	if err != nil {
		return nil, err
	}
	return localPeer{r}, nil
}

// serveHTTP exposes one repository. Each request is its own transaction.
func serveHTTP(root, addr string, out io.Writer) error {
	token := os.Getenv("PLY_TOKEN")
	with := func(fn func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, q *http.Request) {
			if token != "" && q.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "missing or wrong token", 401)
				return
			}
			r, err := openRepo(root)
			if err != nil {
				http.Error(w, err.Error(), 503)
				return
			}
			p := localPeer{r}
			v, code, err := func() (v any, code int, err error) {
				defer func() {
					if x := recover(); x != nil {
						err, code = fmt.Errorf("%v", x), 500
					}
				}()
				return fn(p, w, q)
			}()
			if err != nil {
				r.close()
				http.Error(w, err.Error(), code)
				return
			}
			p.Close()
			if raw, ok := v.([]byte); ok {
				w.Write(raw)
				return
			}
			json.NewEncoder(w).Encode(v)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ref/{name}", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		if _, err := p.r.loadWS(q.PathValue("name")); err != nil {
			return nil, 404, err
		}
		root, _ := p.Ref(q.PathValue("name"))
		return map[string]string{"root": root}, 200, nil
	}))
	mux.HandleFunc("GET /node/{hash}", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		b, err := p.Node(q.PathValue("hash"))
		return b, 404, err
	}))
	mux.HandleFunc("POST /nodes", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		var in struct{ Hashes []string }
		if err := json.NewDecoder(q.Body).Decode(&in); err != nil {
			return nil, 400, err
		}
		ns, err := p.Nodes(in.Hashes)
		return map[string]any{"nodes": ns}, 404, err
	}))
	mux.HandleFunc("POST /lacks", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		var in struct{ Hashes []string }
		if err := json.NewDecoder(q.Body).Decode(&in); err != nil {
			return nil, 400, err
		}
		l, err := p.Lacks(in.Hashes)
		return map[string]any{"lacks": l}, 500, err
	}))
	mux.HandleFunc("POST /putnodes", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		var in struct{ Nodes map[string][]byte }
		if err := json.NewDecoder(http.MaxBytesReader(w, q.Body, 1<<30)).Decode(&in); err != nil {
			return nil, 400, err
		}
		return map[string]any{}, 400, p.PutNodes(in.Nodes)
	}))
	mux.HandleFunc("POST /changes", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		var in struct{ IDs []string }
		if err := json.NewDecoder(q.Body).Decode(&in); err != nil {
			return nil, 400, err
		}
		cs, err := p.Changes(in.IDs)
		return map[string]any{"changes": cs}, 404, err
	}))
	mux.HandleFunc("POST /push/{name}", with(func(p localPeer, w http.ResponseWriter, q *http.Request) (any, int, error) {
		var in struct{ Changes []*Wire }
		if err := json.NewDecoder(http.MaxBytesReader(w, q.Body, 1<<30)).Decode(&in); err != nil {
			return nil, 400, err
		}
		name := q.PathValue("name")
		if strings.HasPrefix(name, "tmp-") || strings.ContainsAny(name, "/\\.: ") {
			return nil, 400, fmt.Errorf("bad workspace name")
		}
		root, err := p.Push(name, in.Changes)
		return map[string]string{"root": root}, 500, err
	}))
	if token == "" && !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
		return fmt.Errorf("refusing to serve on %s without a token: set PLY_TOKEN, or serve on 127.0.0.1", addr)
	}
	fmt.Fprintf(out, "serving %s on http://%s\n", root, addr)
	return http.ListenAndServe(addr, mux)
}

// ===== pull and push =====

// fetching reads state nodes from here when present and from the peer otherwise.
type fetching struct {
	r    *Repo
	p    Peer
	got  map[string][]byte
	memo map[string]*node
	err  error
}

func (f *fetching) getNode(h string) *node {
	if n, ok := f.memo[h]; ok {
		return n
	}
	if f.r.hasNode(h) {
		return f.r.getNode(h)
	}
	b, err := f.p.Node(h)
	var n node
	if err == nil && nodeHash(b) != h {
		err = fmt.Errorf("the remote sent a node that does not match its name")
	}
	if err == nil {
		err = json.Unmarshal(b, &n)
	}
	if err != nil {
		if f.err == nil {
			f.err = err
		}
		return &node{} // empty: the walk ends, and the error is reported after it
	}
	f.got[h], f.memo[h] = b, &n
	return &n
}

func (f *fetching) putNode(n *node) string { return f.r.putNode(n) }

const batch = 200

func (r *Repo) pull(w *Workspace, p Peer, ref string, in Info) (int, error) {
	if _, err := r.settle(w, in); err != nil {
		return 0, err
	}
	remote, err := p.Ref(ref)
	if err != nil {
		return 0, err
	}
	if remote == "" {
		return 0, fmt.Errorf("the remote has no workspace %q", ref)
	}
	f := &fetching{r: r, p: p, got: map[string][]byte{}, memo: map[string]*node{}}
	var want []entry
	trieDiff(f, remote, w.Root, func(e entry) { want = append(want, e) })
	if f.err != nil {
		return 0, f.err
	}
	var ids []string
	for _, e := range want {
		if r.meta(e.ID) == nil {
			ids = append(ids, e.ID)
		}
	}
	for i := 0; i < len(ids); i += batch {
		part := ids[i:min(i+batch, len(ids))]
		cs, err := p.Changes(part)
		if err != nil {
			return 0, err
		}
		if len(cs) != len(part) {
			return 0, fmt.Errorf("the remote answered with %d changes for %d ids", len(cs), len(part))
		}
		for j, c := range cs {
			m, err := r.storeWire(c)
			if err != nil {
				return 0, err
			}
			if m.ID != part[j] {
				return 0, fmt.Errorf("the remote sent a change that does not match its id %s", short(part[j]))
			}
		}
	}
	// Only now, with every change they name in hand, keep the fetched nodes.
	for h, b := range f.got {
		r.putRawNode(h, b)
	}
	// Each change names the set its author was looking at; "did these two
	// agents see each other's work" is answered from it, so it comes along.
	var bases []string
	for _, id := range ids {
		bases = append(bases, r.meta(id).Base)
	}
	if err := r.fetchTries(p, bases); err != nil {
		return 0, err
	}
	w.Root = trieUnion(r, w.Root, remote)
	_, err = r.settle(w, in)
	return len(want), err
}

func (r *Repo) push(w *Workspace, p Peer, ref string, in Info) (int, error) {
	if _, err := r.settle(w, in); err != nil {
		return 0, err
	}
	remote, err := p.Ref(ref)
	if err != nil {
		return 0, err
	}
	f := &fetching{r: r, p: p, got: map[string][]byte{}, memo: map[string]*node{}}
	var send []entry
	trieDiff(f, w.Root, remote, func(e entry) { send = append(send, e) })
	if f.err != nil {
		return 0, f.err
	}
	// Oldest first, so a change always arrives after the changes it builds on.
	sortEntries(send)
	var bases []string
	for _, e := range send {
		bases = append(bases, r.meta(e.ID).Base)
	}
	if err := r.sendTries(p, bases); err != nil {
		return 0, err
	}
	for i := 0; i < len(send); i += batch {
		var cs []*Wire
		for _, e := range send[i:min(i+batch, len(send))] {
			cs = append(cs, r.wire(e.ID))
		}
		if _, err := p.Push(ref, cs); err != nil {
			return i, err
		}
	}
	return len(send), nil
}

func sortEntries(es []entry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].L != es[j].L {
			return es[i].L < es[j].L
		}
		return es[i].ID < es[j].ID
	})
}

func kidsOf(b []byte) []string {
	var n node
	if json.Unmarshal(b, &n) != nil || n.Kids == nil {
		return nil
	}
	var out []string
	for _, k := range n.Kids {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// fetchTries brings over whole state tries, level by level, skipping every
// subtree already here. States share almost all their nodes, so this is small.
func (r *Repo) fetchTries(p Peer, roots []string) error {
	seen := map[string]bool{}
	var frontier []string
	add := func(h string) {
		if h != "" && !seen[h] && !r.hasNode(h) {
			seen[h] = true
			frontier = append(frontier, h)
		}
	}
	for _, h := range roots {
		add(h)
	}
	type got struct {
		h string
		b []byte
	}
	var all []got
	for len(frontier) > 0 {
		level := frontier
		frontier = nil
		for i := 0; i < len(level); i += 500 {
			ns, err := p.Nodes(level[i:min(i+500, len(level))])
			if err != nil {
				return err
			}
			for h, b := range ns {
				if nodeHash(b) != h {
					return fmt.Errorf("the remote sent a node that does not match its name")
				}
				all = append(all, got{h, b})
				for _, k := range kidsOf(b) {
					add(k)
				}
			}
		}
	}
	for i := len(all) - 1; i >= 0; i-- { // deepest first: a stored node always has its children
		r.putRawNode(all[i].h, all[i].b)
	}
	return nil
}

// sendTries is the same walk in the other direction.
func (r *Repo) sendTries(p Peer, roots []string) error {
	seen := map[string]bool{}
	var frontier []string
	for _, h := range roots {
		if h != "" && !seen[h] {
			seen[h] = true
			frontier = append(frontier, h)
		}
	}
	nodes := map[string][]byte{}
	for len(frontier) > 0 {
		var next []string
		for i := 0; i < len(frontier); i += 500 {
			lacks, err := p.Lacks(frontier[i:min(i+500, len(frontier))])
			if err != nil {
				return err
			}
			for _, h := range lacks {
				b := r.rawNode(h)
				nodes[h] = b
				for _, k := range kidsOf(b) {
					if !seen[k] {
						seen[k] = true
						next = append(next, k)
					}
				}
			}
		}
		frontier = next
	}
	if len(nodes) == 0 {
		return nil
	}
	return p.PutNodes(nodes)
}
