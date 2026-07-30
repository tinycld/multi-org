// Package manifesteval evaluates a package's manifest.ts to JSON.
//
// The evaluator itself (Eval) transpiles the manifest to CommonJS and runs it
// in a throwaway sobek VM with a wall-clock interrupt. That bounds TIME, not
// MEMORY: a doubling-string bomb allocates gigabytes in microseconds, and the
// router process hosts every tenant — an OOM here takes them all down.
//
// True isolation therefore runs the eval in a short-lived SUBPROCESS
// (EvalSubprocess ⇄ ServeStdio) that applies an address-space rlimit to
// itself before touching the input: an allocation bomb kills the child, the
// parent reports an error, and every tenant keeps serving. serve-multi wires
// this via its hidden `manifest-eval` subcommand; in-process Eval remains for
// tests and as the child's own engine.
package manifesteval

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

// Timeout bounds one eval's VM run. A manifest is expected to be a pure
// object literal, but that is an assumption about untrusted package JS, not
// an enforced property — without an interrupt, `while(true){}` hangs the
// caller unrecoverably. Generous: a genuine manifest evaluates in
// microseconds. A var only so the runaway-script test doesn't spin a core
// for the full production window.
var Timeout = 5 * time.Second

// Eval transpiles a manifest module to CommonJS and evaluates it in a fresh
// sobek VM, returning the default export as a plain Go value (maps/slices/
// scalars) suitable for json.Marshal. Evaluation runs a pure object literal —
// the manifest has no runtime imports — so the VM needs no host bindings.
func Eval(src, name string) (any, error) {
	res := api.Transform(src, api.TransformOptions{
		Loader: api.LoaderTS,
		Target: api.ES2020,
		Format: api.FormatCommonJS,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("transpile %s: %s", name, res.Errors[0].Text)
	}

	vm := sobek.New()
	module := vm.NewObject()
	exports := vm.NewObject()
	if err := module.Set("exports", exports); err != nil {
		return nil, err
	}
	if err := vm.Set("module", module); err != nil {
		return nil, err
	}
	if err := vm.Set("exports", exports); err != nil {
		return nil, err
	}

	interrupt := time.AfterFunc(Timeout, func() {
		vm.Interrupt(fmt.Sprintf("manifest evaluation exceeded %s", Timeout))
	})
	defer interrupt.Stop()

	if _, err := vm.RunString(string(res.Code)); err != nil {
		return nil, err
	}

	// CommonJS default export lands on module.exports.default (esbuild) or, for a
	// bare `module.exports =`, on module.exports itself.
	me := module.Get("exports")
	if me == nil {
		return nil, fmt.Errorf("manifest produced no exports")
	}
	meObj := me.ToObject(vm)
	if def := meObj.Get("default"); def != nil && !sobek.IsUndefined(def) {
		return def.Export(), nil
	}
	return me.Export(), nil
}

// EvalJSON is Eval marshalled: the shape both the in-process default and the
// subprocess child produce, so callers see one contract.
func EvalJSON(src, name string) ([]byte, error) {
	obj, err := Eval(src, name)
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", name, err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest json: %w", err)
	}
	return data, nil
}
