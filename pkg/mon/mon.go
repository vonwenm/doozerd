package mon

import (
	"junta/store"
	"junta/util"
	"log"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	ctlKey    = "/mon/ctl"
	defKey    = "/mon/def"
	lockKey   = "/lock"
	statusKey = "/mon/status"

	ctlDir    = ctlKey + "/"
	defDir    = defKey + "/"
	lockDir   = lockKey + "/"
	statusDir = statusKey + "/"
)

type unit interface {
	dispatchLockEvent(ev store.Event)
	start()
	stop()
	tick()
}

type exec interface {
	unit
	exited(w *os.Waitmsg)
}

type ready struct {
	r readyer
	f *os.File
}

type readyer interface {
	ready(f *os.File)
}

type exit struct {
	e exec
	w *os.Waitmsg
}

type SetDeler interface {
	Set(p, body, cas string) (seqn uint64, err os.Error)
	Del(p, cas string) (seqn uint64, err os.Error)
}

type monitor struct {
	self, prefix string
	host         string
	st           *store.Store
	cl           SetDeler
	clock        chan unit
	units        map[string]unit
	refs         map[string]int
	exitCh       chan exit
	readyCh      chan ready
	logger       *log.Logger
}

func splitId(id string) (name, ext string) {
	parts := strings.Split(id, ".", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func Monitor(self, prefix string, st *store.Store, cl SetDeler) os.Error {

	mon := &monitor{
		self:   self,
		prefix: prefix,
		host:   os.Getenv("HOSTNAME"),
		st:     st,
		cl:     cl,
		clock:  make(chan unit),
		units:  make(map[string]unit),
		refs:   make(map[string]int),
		exitCh: make(chan exit),
		readyCh:make(chan ready),
		logger: util.NewLogger("monitor"),
	}

	mon.logger.Log("reading units")
	evs := make(chan store.Event)
	st.Watch(ctlKey+"/*", evs)
	st.Watch(lockKey+"/*", evs)
	go func() {
		for _, id := range st.LookupDir(ctlKey) {
			p := ctlDir + id
			v, cas := st.Lookup(p)
			if cas != store.Dir && cas != store.Missing {
				mon.logger.Log("injecting", id)
				evs <- store.Event{0, p, v[0], cas, "", nil}
			}
		}
	}()

	for {
		select {
		case u := <-mon.clock:
			u.tick()
		case ev := <-evs:
			prefix, id := path.Split(ev.Path)
			switch prefix {
			case ctlDir:
				if ev.IsDel() {
					mon.decrefUnit(id)
					break
				}

				ut := mon.increfUnit(id)
				if ut == nil {
					break
				}

				switch ev.Body {
				case "start":
					ut.start()
				case "stop":
					ut.stop()
				case "auto", "":
					fallthrough
				default:
					// nothing
				}

			case lockDir:
				ut := mon.units[id]
				if ut == nil {
					break
				}

				ut.dispatchLockEvent(ev)
			}
		case e := <-mon.exitCh:
			e.e.exited(e.w)
		case r := <-mon.readyCh:
			r.r.ready(r.f)
		}
	}
	panic("unreachable")
}

func (mon *monitor) newUnit(id string) unit {
	name, ext := splitId(id)
	switch ext {
	case "service":
		return newService(id, name, mon)
	case "socket":
		return newSocket(id, name, mon)
	}
	return nil
}

func (mon *monitor) lookupParam(id, param string) string {
	return mon.st.LookupString(defDir + id + "/" + param)
}

func (mon *monitor) setStatus(id, param, val string) {
	mon.cl.Set(mon.prefix+statusKey+"/"+id+"/"+param, val, store.Clobber)
}

func (mon *monitor) delStatus(id, param string) {
	mon.cl.Del(mon.prefix+statusKey+"/"+id+"/"+param, store.Clobber)
}

func (mon *monitor) tryLock(id string) {
	mon.cl.Set(mon.prefix+lockKey+"/"+id, mon.self, store.Missing)
}

func (mon *monitor) release(id, cas string) {
	mon.cl.Del(mon.prefix+lockKey+"/"+id, cas)
}

// `p` should look like `/foo/bar/name.ext
func splitPath(p string) (id string) {
	_, id = path.Split(p)
	return
}

func (mon *monitor) timer(u unit, ns int64) {
	time.Sleep(ns)
	mon.clock <- u
}

func (mon *monitor) wait(pid int, e exec) {
	w, err := os.Wait(pid, 0)
	if err != nil {
		mon.logger.Log(err)
		return
	}

	mon.exitCh <- exit{e, w}
}

// This can be optimized to use a single epoll server for the entire process,
// similar to what they do in package net. For now, we'll just live with the
// extra os thread.
func (mon *monitor) poll(files []*os.File, r readyer) {
	var n int
	var rd syscall.FdSet
	for _, f := range files {
		fdAdd(&rd, f.Fd())
		if f.Fd() > n {
			n = f.Fd()
		}
	}

	errno := selectFds(n+1, &rd, nil, nil, nil)
	if errno != 0 {
		mon.logger.Log("select", os.Errno(errno))
		return
	}

	for _, f := range files {
		if fdIsSet(&rd, f.Fd()) {
			mon.readyCh <- ready{r, f}
		}
	}
}

func (mon *monitor) increfUnit(id string) unit {
	ut := mon.units[id]
	if ut == nil {
		ut = mon.newUnit(id)
		if ut == nil {
			return nil
		}
		mon.units[id] = ut
	}
	mon.refs[id]++
	return ut
}

func (mon *monitor) decrefUnit(id string) {
	ut := mon.units[id]
	if ut == nil {
		return
	}
	mon.refs[id]--
	if mon.refs[id] < 1 {
		ut.stop()
		mon.units[id] = nil, false
		mon.refs[id] = 0, false
	}
}

func (mon *monitor) increfService(id string) *service {
	sv, ok := mon.increfUnit(id).(*service)
	if !ok {
		mon.decrefUnit(id)
	}
	return sv
}
