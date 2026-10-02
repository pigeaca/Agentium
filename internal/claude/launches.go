package claude

import "time"

// mainLaunch is the main session's key among launches: no Agent call has this ID, and an empty parent_tool_use_id
// stays a launch of unknown type instead of joining the main session.
const mainLaunch = "\x00main"

// syntheticModel is the model Claude Code names on messages it makes up itself (an interruption notice, say): they
// are not model requests.
const syntheticModel = "<synthetic>"

// launchLog collects, while Parse reads a transcript, what the isolated-run cost needs: each launch's requests (the
// main session, and each subagent launch under its Agent call), the Agent calls, and when and where in the stream each
// happened. It is in memory only; Metrics keeps its result (FirstReads), which names no subagent type.
type launchLog struct {
	calls    map[string]agentCall // Agent (Task) calls by tool-use ID
	launches map[string]*launch   // by mainLaunch or the Agent call's ID
	order    []*launch            // in the order their first real requests arrived
}

// agentCall is an Agent (Task) tool call: the subagent type it launched, and where it is in the stream.
type agentCall struct {
	kind string
	line int       // the transcript line that carried it
	at   time.Time // its event's timestamp; zero when the event had none
}

// launch is the main session or one subagent launch.
type launch struct {
	key      string
	first    *request    // its first real request
	firstAt  time.Time   // that request's first event's timestamp; zero when missing
	requests []*request  // its real requests, each once, in arrival order
	events   []eventSeen // every event of its real requests
}

// eventSeen is one assistant event of a real request: a request that read or wrote the launch's cache prefix.
type eventSeen struct {
	line int
	at   time.Time
}

func newLaunchLog() *launchLog {
	return &launchLog{calls: map[string]agentCall{}, launches: map[string]*launch{}}
}

// call records an Agent call.
func (l *launchLog) call(id, kind string, line int, at time.Time) {
	if _, ok := l.calls[id]; !ok {
		l.calls[id] = agentCall{kind: kind, line: line, at: at}
	}
}

// request records an assistant event of req, under parent (nil for the main session). Only real requests count: a
// message with input, cache writes or cache reads, on a model that is not Claude Code's synthetic one. A message
// without usage, or "usage": null, is not one.
func (l *launchLog) request(parent *string, req *request, u requestUsage, usageOK bool, model string, line int, at time.Time) {
	if !usageOK || model == syntheticModel || u.Input+u.CacheCreation+u.CacheRead <= 0 {
		return
	}
	key := mainLaunch
	if parent != nil {
		key = *parent
	}
	ln := l.launches[key]
	if ln == nil {
		ln = &launch{key: key, first: req, firstAt: at}
		l.launches[key] = ln
		l.order = append(l.order, ln)
	}
	if len(ln.requests) == 0 || ln.requests[len(ln.requests)-1] != req {
		ln.requests = append(ln.requests, req)
	}
	ln.events = append(ln.events, eventSeen{line: line, at: at})
}

// observedTTL is the time to live of the launch's first request that wrote the cache with a reported split: TTL1h when
// it wrote any one-hour entry, else TTL5m; empty when no request of the launch reported a split write.
func (ln *launch) observedTTL() string {
	for _, r := range ln.requests {
		if !r.split || r.write5m+r.write1h == 0 {
			continue
		}
		if r.write1h > 0 {
			return TTL1h
		}
		return TTL5m
	}
	return ""
}

// ttlDuration is how long a cache entry written at ttl lives without a read.
func ttlDuration(ttl string) time.Duration {
	if ttl == TTL1h {
		return time.Hour
	}
	return 5 * time.Minute
}

// firstReads lists the requests the isolated-run cost reprices (Metrics.FirstReads) and counts the subagent launches of
// unknown type. The main session's first request comes first. A subagent launch follows, unless it repeats an earlier
// launch of its type: one with a request before this launch's Agent call in the stream, and at most that launch's
// write time to live before this launch's first request. Such a launch reads the prefix the run wrote itself. Parallel
// launches of a type (no request of one before the other's call), a launch after the prefix expired, and a launch
// whose timing is unknown (a missing timestamp) are repriced: a missing fact never hides a read.
func (l *launchLog) firstReads() ([]FirstRead, int) {
	main := l.launches[mainLaunch]
	if main == nil {
		return nil, 0 // no real main-session request: nothing to start from
	}
	// A launch that wrote nothing reported takes the time to live its type wrote with elsewhere in the run, then the
	// default: five minutes for subagents, one hour for the main session, as recorded runs wrote.
	typeTTL := map[string]string{}
	for _, ln := range l.order {
		if call, ok := l.calls[ln.key]; ok && typeTTL[call.kind] == "" {
			typeTTL[call.kind] = ln.observedTTL()
		}
	}
	resolve := func(ln *launch, kind string) (string, bool) {
		if ttl := ln.observedTTL(); ttl != "" {
			return ttl, false
		}
		switch {
		case ln.key == mainLaunch:
			return TTL1h, true
		case typeTTL[kind] != "":
			return typeTTL[kind], true
		}
		return TTL5m, true
	}
	entry := func(ln *launch, kind string) FirstRead {
		ttl, assumed := resolve(ln, kind)
		return FirstRead{Main: ln.key == mainLaunch, Model: ln.first.model, CacheRead: ln.first.read, WriteTTL: ttl, TTLAssumed: assumed}
	}
	reads := []FirstRead{entry(main, "")}
	unmatched := 0
	for i, ln := range l.order {
		if ln.key == mainLaunch {
			continue
		}
		call, ok := l.calls[ln.key]
		if !ok {
			unmatched++
			continue
		}
		if !l.repeats(ln, call, l.order[:i], resolve) {
			reads = append(reads, entry(ln, call.kind))
		}
	}
	return reads, unmatched
}

// repeats reports whether launch ln, made by call, found its type's prefix written by this run: an earlier launch of
// the type had a request before the call, at most its write time to live before ln's first request.
func (l *launchLog) repeats(ln *launch, call agentCall, earlier []*launch, resolve func(*launch, string) (string, bool)) bool {
	if ln.firstAt.IsZero() {
		return false
	}
	for _, e := range earlier {
		if c, ok := l.calls[e.key]; !ok || c.kind != call.kind {
			continue
		}
		ttl, _ := resolve(e, call.kind)
		for _, ev := range e.events {
			if ev.line < call.line && !ev.at.IsZero() && !ev.at.After(ln.firstAt) && ln.firstAt.Sub(ev.at) <= ttlDuration(ttl) {
				return true
			}
		}
	}
	return false
}
