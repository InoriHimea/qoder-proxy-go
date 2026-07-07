package wasm

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
)

var debugImports = os.Getenv("WASM_DEBUG_IMPORTS") != ""

var callCounters = map[string]*int64{}

func debugCount(name string) {
	if !debugImports {
		return
	}
	c, ok := callCounters[name]
	if !ok {
		v := int64(0)
		c = &v
		callCounters[name] = c
	}
	n := atomic.AddInt64(c, 1)
	if n%100000 == 0 || n < 20 {
		fmt.Fprintf(os.Stderr, "[wasm-import] %s called %d times\n", name, n)
	}
}

func debugArgs(name string, n int64, format string, args ...interface{}) {
	if !debugImports || n >= 20 {
		return
	}
	fmt.Fprintf(os.Stderr, "[wasm-args] %s(%d): "+format+"\n", append([]interface{}{name, n}, args...)...)
}

func debugN(name string) int64 {
	c, ok := callCounters[name]
	if !ok {
		return 0
	}
	return atomic.LoadInt64(c)
}

func firstN(b []byte, n int) []byte {
	if len(b) < n {
		n = len(b)
	}
	return b[:n]
}

func i32s(n int) []api.ValueType {
	v := make([]api.ValueType, n)
	for i := range v {
		v[i] = api.ValueTypeI32
	}
	return v
}

// registerImports registers all 31 wasm-bindgen glue imports as host functions.
func (r *Runtime) registerImports() error {
	ref := r.refTab

	reg := r.runtime.NewHostModuleBuilder("./qoder_auth_wasm_bg.js")

	// __wbg_Error_2e59b1b37a9a34c3  params=[i32,i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_Error_2e59b1b37a9a34c3")
		s := r.readString(uint32(stack[0]), uint32(stack[1]))
		stack[0] = uint64(ref.intern(&Obj{kind: ObjError, str: s}))
	}), i32s(2), i32s(1)).Export("__wbg_Error_2e59b1b37a9a34c3")

	// __wbg___wbindgen_is_function_49868bde5eb1e745  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg___wbindgen_is_function_49868bde5eb1e745")
		o := ref.resolve(uint32(stack[0]))
		if o != nil && o.kind == ObjFunction {
			stack[0] = 1
		} else {
			stack[0] = 0
		}
	}), i32s(1), i32s(1)).Export("__wbg___wbindgen_is_function_49868bde5eb1e745")

	// __wbg___wbindgen_is_object_40c5a80572e8f9d3  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg___wbindgen_is_object_40c5a80572e8f9d3")
		o := ref.resolve(uint32(stack[0]))
		if o != nil && (o.kind == ObjMap || o.kind == ObjUint8Array || o.kind == ObjGlobalThis || o.kind == ObjCrypto || o.kind == ObjWindow || o.kind == ObjProcess) {
			stack[0] = 1
		} else {
			stack[0] = 0
		}
	}), i32s(1), i32s(1)).Export("__wbg___wbindgen_is_object_40c5a80572e8f9d3")

	// __wbg___wbindgen_is_string_b29b5c5a8065ba1a  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg___wbindgen_is_string_b29b5c5a8065ba1a")
		o := ref.resolve(uint32(stack[0]))
		if o != nil && o.kind == ObjString {
			stack[0] = 1
		} else {
			stack[0] = 0
		}
	}), i32s(1), i32s(1)).Export("__wbg___wbindgen_is_string_b29b5c5a8065ba1a")

	// __wbg___wbindgen_is_undefined_c0cca72b82b86f4d  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d")
		o := ref.resolve(uint32(stack[0]))
		if o != nil && o.kind == ObjUndefined {
			stack[0] = 1
		} else {
			stack[0] = 0
		}
	}), i32s(1), i32s(1)).Export("__wbg___wbindgen_is_undefined_c0cca72b82b86f4d")

	// __wbg___wbindgen_throw_81fc77679af83bc6  params=[i32,i32] results=[]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg___wbindgen_throw_81fc77679af83bc6")
		_ = r.readString(uint32(stack[0]), uint32(stack[1]))
	}), i32s(2), i32s(0)).Export("__wbg___wbindgen_throw_81fc77679af83bc6")

	// __wbg_call_d578befcc3145dee  params=[i32,i32,i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_call_d578befcc3145dee")
		stack[0] = 0
	}), i32s(3), i32s(1)).Export("__wbg_call_d578befcc3145dee")

	// __wbg_crypto_38df2bab126b63dc  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_crypto_38df2bab126b63dc")
		stack[0] = uint64(ref.intern(&Obj{kind: ObjCrypto}))
	}), i32s(1), i32s(1)).Export("__wbg_crypto_38df2bab126b63dc")

	// __wbg_getRandomValues_c44a50d8cfdaebeb  params=[i32,i32] results=[]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_getRandomValues_c44a50d8cfdaebeb")
		arr := ref.resolve(uint32(stack[1]))
		n := debugN("__wbg_getRandomValues_c44a50d8cfdaebeb")
		if arr != nil && arr.kind == ObjUint8Array && arr.data != nil {
			r.fillRandom(arr.data)
			debugArgs("__wbg_getRandomValues_c44a50d8cfdaebeb", n, "ref=%d dataLen=%d first4=%v", uint32(stack[1]), len(arr.data), firstN(arr.data, 4))
		} else {
			debugArgs("__wbg_getRandomValues_c44a50d8cfdaebeb", n, "ref=%d NOT fillable (nil=%v kind=%v)", uint32(stack[1]), arr == nil, func() int {
				if arr != nil {
					return int(arr.kind)
				}
				return -1
			}())
		}
	}), i32s(2), i32s(0)).Export("__wbg_getRandomValues_c44a50d8cfdaebeb")

	// __wbg_getRandomValues_d49329ff89a07af1  params=[i32,i32] results=[]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_getRandomValues_d49329ff89a07af1")
		arr := ref.resolve(uint32(stack[1]))
		if arr != nil && arr.kind == ObjUint8Array && arr.data != nil {
			r.fillRandom(arr.data)
		}
	}), i32s(2), i32s(0)).Export("__wbg_getRandomValues_d49329ff89a07af1")

	// __wbg_length_0c32cb8543c8e4c8  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_length_0c32cb8543c8e4c8")
		o := ref.resolve(uint32(stack[0]))
		if o == nil {
			stack[0] = 0
			return
		}
		switch o.kind {
		case ObjString:
			stack[0] = uint64(len([]rune(o.str)))
		case ObjUint8Array:
			stack[0] = uint64(len(o.data))
		case ObjMap:
			stack[0] = uint64(len(o.members))
		default:
			stack[0] = 0
		}
	}), i32s(1), i32s(1)).Export("__wbg_length_0c32cb8543c8e4c8")

	// __wbg_msCrypto_bd5a034af96bcba6  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_msCrypto_bd5a034af96bcba6")
		stack[0] = 0
	}), i32s(1), i32s(1)).Export("__wbg_msCrypto_bd5a034af96bcba6")

	// __wbg_new_99cabae501c0a8a0  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_new_99cabae501c0a8a0")
		stack[0] = uint64(ref.intern(&Obj{kind: ObjMap, members: make(map[string]*Obj)}))
	}), i32s(0), i32s(1)).Export("__wbg_new_99cabae501c0a8a0")

	// __wbg_new_with_length_9cedd08484b73942  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_new_with_length_9cedd08484b73942")
		size := uint32(stack[0])
		o := &Obj{kind: ObjUint8Array, data: make([]byte, int(size))}
		ret := ref.intern(o)
		debugArgs("__wbg_new_with_length_9cedd08484b73942", debugN("__wbg_new_with_length_9cedd08484b73942"), "size=%d ref=%d", size, ret)
		stack[0] = uint64(ret)
	}), i32s(1), i32s(1)).Export("__wbg_new_with_length_9cedd08484b73942")

	// __wbg_node_84ea875411254db1  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_node_84ea875411254db1")
		stack[0] = 0
	}), i32s(1), i32s(1)).Export("__wbg_node_84ea875411254db1")

	// __wbg_now_88621c9c9a4f3ffc  params=[] results=[f64]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_now_88621c9c9a4f3ffc")
		stack[0] = math.Float64bits(float64(time.Now().UnixMilli()))
	}), i32s(0), []api.ValueType{api.ValueTypeF64}).Export("__wbg_now_88621c9c9a4f3ffc")

	// __wbg_process_44c7a14e11e9f69e  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		debugCount("__wbg_process_44c7a14e11e9f69e")
		proc := &Obj{kind: ObjProcess, members: make(map[string]*Obj)}
		proc.members["env"] = &Obj{kind: ObjMap, members: make(map[string]*Obj)}
		proc.members["versions"] = &Obj{kind: ObjMap, members: map[string]*Obj{
			"node": {kind: ObjString, str: "18.17.0"},
		}}
		stack[0] = uint64(ref.intern(proc))
	}), i32s(1), i32s(1)).Export("__wbg_process_44c7a14e11e9f69e")

	// __wbg_prototypesetcall_3e05eb9545565046  params=[i32,i32,i32] results=[]
	// Real JS: Uint8Array.prototype.set.call(QgA(A,t), Dc(n))
	// where QgA(ptr,len) = wasm memory.subarray(ptr, ptr+len) — i.e. A,t are a
	// raw (ptr,len) view directly into WASM LINEAR MEMORY, not a ref-table
	// handle. n is the ref of the source Uint8Array to copy FROM. Writing the
	// copied bytes straight into wasm memory (instead of into some unrelated
	// ref'd Obj) is required for callers like getRandomValues(buf.subarray(...))
	// to ever observe the fill — this was the root cause of the infinite loop.
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_prototypesetcall_3e05eb9545565046")
		ptr := uint32(stack[0])
		length := uint32(stack[1])
		src := ref.resolve(uint32(stack[2]))
		n := debugN("__wbg_prototypesetcall_3e05eb9545565046")
		if src != nil && src.kind == ObjUint8Array {
			toCopy := len(src.data)
			if uint32(toCopy) > length {
				toCopy = int(length)
			}
			ok := m.Memory().Write(ptr, src.data[:toCopy])
			debugArgs("__wbg_prototypesetcall_3e05eb9545565046", n, "ptr=%d len=%d srcRef=%d srcLen=%d wrote=%v", ptr, length, uint32(stack[2]), len(src.data), ok)
		} else {
			debugArgs("__wbg_prototypesetcall_3e05eb9545565046", n, "ptr=%d len=%d srcRef=%d skipped (srcNil=%v)", ptr, length, uint32(stack[2]), src == nil)
		}
	}), i32s(3), i32s(0)).Export("__wbg_prototypesetcall_3e05eb9545565046")

	// __wbg_randomFillSync_6c25eac9869eb53c  params=[i32,i32] results=[]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_randomFillSync_6c25eac9869eb53c")
	}), i32s(2), i32s(0)).Export("__wbg_randomFillSync_6c25eac9869eb53c")

	// __wbg_require_b4edbdcf3e2a1ef0  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_require_b4edbdcf3e2a1ef0")
		stack[0] = 0
	}), i32s(0), i32s(1)).Export("__wbg_require_b4edbdcf3e2a1ef0")

	// __wbg_set_08463b1df38a7e29  params=[i32,i32,i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_set_08463b1df38a7e29")
		mm := ref.resolve(uint32(stack[0]))
		if mm == nil || mm.kind != ObjMap {
			stack[0] = 0
			return
		}
		if mm.members == nil {
			mm.members = make(map[string]*Obj)
		}
		k := ref.resolve(uint32(stack[1]))
		v := ref.resolve(uint32(stack[2]))
		mm.members[objToKey(k)] = v
		stack[0] = 0
	}), i32s(3), i32s(1)).Export("__wbg_set_08463b1df38a7e29")

	// __wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f")
		stack[0] = uint64(ref.intern(&Obj{kind: ObjGlobalThis, members: make(map[string]*Obj)}))
	}), i32s(0), i32s(1)).Export("__wbg_static_accessor_GLOBAL_THIS_a1248013d790bf5f")

	// __wbg_static_accessor_GLOBAL_f2e0f995a21329ff  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_static_accessor_GLOBAL_f2e0f995a21329ff")
		stack[0] = 0
	}), i32s(0), i32s(1)).Export("__wbg_static_accessor_GLOBAL_f2e0f995a21329ff")

	// __wbg_static_accessor_SELF_24f78b6d23f286ea  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_static_accessor_SELF_24f78b6d23f286ea")
		stack[0] = uint64(ref.intern(&Obj{kind: ObjGlobalThis, members: make(map[string]*Obj)}))
	}), i32s(0), i32s(1)).Export("__wbg_static_accessor_SELF_24f78b6d23f286ea")

	// __wbg_static_accessor_WINDOW_59fd959c540fe405  params=[] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_static_accessor_WINDOW_59fd959c540fe405")
		stack[0] = 0
	}), i32s(0), i32s(1)).Export("__wbg_static_accessor_WINDOW_59fd959c540fe405")

	// __wbg_subarray_0f98d3fb634508ad  params=[i32,i32,i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_subarray_0f98d3fb634508ad")
		arr := ref.resolve(uint32(stack[0]))
		if arr == nil || arr.kind != ObjUint8Array {
			debugArgs("__wbg_subarray_0f98d3fb634508ad", debugN("__wbg_subarray_0f98d3fb634508ad"), "src ref=%d NOT a Uint8Array (nil=%v)", uint32(stack[0]), arr == nil)
			stack[0] = 0
			return
		}
		off := int(uint32(stack[1]))
		last := int(uint32(stack[2]))
		origOff, origLast, srcLen := off, last, len(arr.data)
		if off < 0 {
			off = 0
		}
		if last > len(arr.data) {
			last = len(arr.data)
		}
		if off > last {
			off, last = 0, 0
		}
		// share backing array (a real Uint8Array.subarray is a VIEW, not a copy) so
		// writes through the subview, e.g. getRandomValues(buf.subarray(a,b)),
		// are visible on the parent Obj's data too.
		sub := arr.data[off:last:last]
		ret := ref.intern(&Obj{kind: ObjUint8Array, data: sub})
		debugArgs("__wbg_subarray_0f98d3fb634508ad", debugN("__wbg_subarray_0f98d3fb634508ad"), "srcRef=%d srcLen=%d off=%d last=%d -> clampedOff=%d clampedLast=%d subLen=%d retRef=%d", uint32(stack[0]), srcLen, origOff, origLast, off, last, len(sub), ret)
		stack[0] = uint64(ret)
	}), i32s(3), i32s(1)).Export("__wbg_subarray_0f98d3fb634508ad")

	// __wbg_versions_276b2795b1c6a219  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbg_versions_276b2795b1c6a219")
		stack[0] = 0
	}), i32s(1), i32s(1)).Export("__wbg_versions_276b2795b1c6a219")

	// __wbindgen_cast_0000000000000001  params=[i32,i32] results=[i32]
	// Casts a (ptr,len) linear-memory slice into a Uint8Array ref (copies out
	// of wasm memory since our Obj.data is host-owned).
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbindgen_cast_0000000000000001")
		ptr, length := uint32(stack[0]), uint32(stack[1])
		mem := r.mod.Memory()
		buf, ok := mem.Read(ptr, length)
		if !ok {
			stack[0] = 0
			return
		}
		cp := make([]byte, len(buf))
		copy(cp, buf)
		stack[0] = uint64(ref.intern(&Obj{kind: ObjUint8Array, data: cp}))
	}), i32s(2), i32s(1)).Export("__wbindgen_cast_0000000000000001")

	// __wbindgen_cast_0000000000000002  params=[i32,i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbindgen_cast_0000000000000002")
		s := r.readString(uint32(stack[0]), uint32(stack[1]))
		stack[0] = uint64(ref.intern(&Obj{kind: ObjString, str: s}))
	}), i32s(2), i32s(1)).Export("__wbindgen_cast_0000000000000002")

	// __wbindgen_object_clone_ref  params=[i32] results=[i32]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbindgen_object_clone_ref")
		stack[0] = uint64(ref.cloneRef(uint32(stack[0])))
	}), i32s(1), i32s(1)).Export("__wbindgen_object_clone_ref")

	// __wbindgen_object_drop_ref  params=[i32] results=[]
	reg.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, m api.Module, stack []uint64) {
		debugCount("__wbindgen_object_drop_ref")
	}), i32s(1), i32s(0)).Export("__wbindgen_object_drop_ref")

	_, err := reg.Instantiate(r.ctx)
	return err
}
