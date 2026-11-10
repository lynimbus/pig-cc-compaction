package pigcc

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/MoeForge/moejs"
)

// ModuleName is the name the plugin source compiles under: it shows up in
// JavaScript stack traces and in Compile's errors.
const ModuleName = "plugin/compaction.js"

// The two hooks plugin/compaction.js exports.
const (
	planHookName  = "planCompact"
	parseHookName = "parseSummary"
)

// compactionJS is plugin/compaction.js, embedded so the binary carries the
// policy it runs: the plugin file is the source of truth, and no Go change is
// needed to reword the prompt.
//
//go:embed plugin/compaction.js
var compactionJS string

// DefaultSource returns the embedded plugin/compaction.js.
func DefaultSource() string { return compactionJS }

// MoePlugin is the JS core of the compaction policy: one compiled module, and
// a pool of moejs runtimes that each loaded it once.
//
// A moejs runtime is single threaded but not goroutine safe, so a call borrows
// one for its whole duration; the pool keeps the cost of a fresh realm off the
// common path. The module is immutable, so any number of runtimes may load it
// concurrently.
type MoePlugin struct {
	name        string
	mod         *moejs.Module
	plan, parse moejs.Hook
	idle        chan *moejs.Runtime
}

var _ Plugin = (*MoePlugin)(nil)

// NewPlugin compiles src once and resolves the two hooks it must export. An
// empty src means DefaultSource(), an empty name means ModuleName.
//
// A source that does not compile, or that lacks planCompact or parseSummary, is
// an error: the plugin is unusable, and failing here beats failing on every
// compaction.
func NewPlugin(name, src string) (*MoePlugin, error) {
	if name == "" {
		name = ModuleName
	}
	if src == "" {
		src = DefaultSource()
	}
	mod, err := moejs.Compile(name, src)
	if err != nil {
		return nil, fmt.Errorf("pigcc: compile %s: %w", name, err)
	}
	plan, err := mod.Hook(planHookName)
	if err != nil {
		return nil, fmt.Errorf("pigcc: %s: %w", name, err)
	}
	parse, err := mod.Hook(parseHookName)
	if err != nil {
		return nil, fmt.Errorf("pigcc: %s: %w", name, err)
	}
	return &MoePlugin{
		name:  name,
		mod:   mod,
		plan:  plan,
		parse: parse,
		idle:  make(chan *moejs.Runtime, poolSize()),
	}, nil
}

// poolSize bounds the idle runtimes a plugin keeps. A runtime is a realm, so
// the pool stays small; callers beyond it get a fresh runtime and it is dropped
// when the pool is full again.
func poolSize() int { return max(4, runtime.NumCPU()) }

// Plan runs planCompact: whether to resend the captured request, and with what.
func (p *MoePlugin) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	var out PlanOutput
	err := p.call(ctx, p.plan, in, &out)
	return out, err
}

// Parse runs parseSummary: the summary in the reply, or why there is none.
func (p *MoePlugin) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	var out ParseOutput
	err := p.call(ctx, p.parse, in, &out)
	return out, err
}

// call runs one hook on a borrowed runtime: marshal in to JSON, hand it to
// JavaScript, decode the result into out.
//
// Anything JavaScript throws and anything that cannot be encoded or decoded is
// an error. A context that ends while the hook runs interrupts the JavaScript;
// the runtime is cleared and only then returned to the pool, so a call that
// outlived its context cannot poison the next one.
func (p *MoePlugin) call(ctx context.Context, hook moejs.Hook, in, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Check first: a context that is already over never runs the policy, and
	// returning its error is deterministic rather than a race with the hook.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pigcc: %s: %w", hook.Name(), err)
	}

	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("pigcc: %s: encode input: %w", hook.Name(), err)
	}

	rt, err := p.get()
	if err != nil {
		return err
	}
	defer p.put(rt)

	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		rt.Interrupt(context.Cause(ctx))
		close(interrupted)
	})
	// Interrupt must have returned before the runtime goes back to the pool.
	// This defer is registered after p.put's, so it runs before it.
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()

	// ParseJSONString reads the text in place: the argument's strings share the
	// marshalled buffer, which lives until this call returns.
	arg, err := rt.ParseJSONString(string(body))
	if err != nil {
		return fmt.Errorf("pigcc: %s: parse input: %w", hook.Name(), err)
	}
	res, err := rt.Call(hook, arg)
	if err != nil {
		return fmt.Errorf("pigcc: %s: %w", hook.Name(), err)
	}
	// A hook that returns nothing has decided nothing: decoding null or
	// undefined would leave out at its zero value and hide the bug.
	if res.IsNullish() {
		return fmt.Errorf("pigcc: %s: the hook returned no result", hook.Name())
	}
	// Unmarshal decodes the result value straight into out, no JSON text hop
	// needed for the plain objects both hooks return.
	if err := rt.Unmarshal(res, out); err != nil {
		return fmt.Errorf("pigcc: %s: decode result: %w", hook.Name(), err)
	}
	return nil
}

// get borrows an idle runtime or makes one: host functions first (none yet),
// then the module.
func (p *MoePlugin) get() (*moejs.Runtime, error) {
	select {
	case rt := <-p.idle:
		return rt, nil
	default:
	}
	rt := moejs.NewRuntime(moejs.Options{})
	if err := rt.Load(p.mod); err != nil {
		return nil, fmt.Errorf("pigcc: load %s: %w", p.name, err)
	}
	return rt, nil
}

// put clears what the call left behind and gives the runtime back, dropping it
// when the pool is full.
func (p *MoePlugin) put(rt *moejs.Runtime) {
	// A context that ended just as the hook returned leaves an interrupt
	// pending: drop it, or the next call that borrows this runtime fails.
	rt.ClearInterrupt()
	// The idle runtime lets go of this call's arguments and their strings.
	rt.ReleaseCallData()
	select {
	case p.idle <- rt:
	default: // the pool is full: let this runtime go
	}
}
