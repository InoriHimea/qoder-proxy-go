package wasm

import "fmt"

// Context wraps a QoderContext instance living inside WASM linear memory.
// It is the Go equivalent of the JS `HR = new n.QoderContext(...)` handle.
type Context struct {
	rt  *Runtime
	ptr uint32
}

// RequestResult wraps a RequestResult instance returned by prepareRequest /
// prepareInferRequest. Callers must call Free() once done (mirrors the JS
// `finally { r.free() }` pattern) to release the WASM-side allocation.
type RequestResult struct {
	rt  *Runtime
	ptr uint32
}

// addToStackPointer adjusts wasm-bindgen's internal stack pointer by delta
// and returns the resulting pointer, matching the JS
// `oi.__wbindgen_add_to_stack_pointer(delta)` calling convention.
func (r *Runtime) addToStackPointer(delta int32) (uint32, error) {
	fn := r.mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	if fn == nil {
		return 0, fmt.Errorf("__wbindgen_add_to_stack_pointer not found")
	}
	results, err := fn.Call(r.ctx, uint64(uint32(delta)))
	if err != nil {
		return 0, err
	}
	return uint32(results[0]), nil
}

// packString allocates and writes s into WASM memory, returning ptr,len.
// If s is nil (representing a JS `undefined`/optional argument), returns 0,0
// without allocating, matching the `ub(o) ? 0 : pB(o, ...)` JS pattern.
func (r *Runtime) packString(s *string) (uint32, uint32, error) {
	if s == nil {
		return 0, 0, nil
	}
	ptr, err := r.writeString(*s)
	if err != nil {
		return 0, 0, err
	}
	return ptr, uint32(len(*s)), nil
}

// readRetString reads a (ptr,len) pair from the stack-return area at
// retptr+offset*4 and retptr+(offset+1)*4, decodes it as a WASM-owned string,
// and frees the underlying WASM allocation (mirrors JS `$w(n,o)` followed by
// `__wbindgen_export4(n,o,1)`).
func (r *Runtime) readRetString(retptr uint32, offset int) (string, error) {
	mem := r.mod.Memory()
	ptr, ok := mem.ReadUint32Le(retptr + uint32(offset)*4)
	if !ok {
		return "", fmt.Errorf("readRetString: out of bounds ptr read")
	}
	length, ok := mem.ReadUint32Le(retptr + uint32(offset+1)*4)
	if !ok {
		return "", fmt.Errorf("readRetString: out of bounds len read")
	}
	if ptr == 0 {
		return "", nil
	}
	s := r.readString(ptr, length)
	r.free(ptr, length)
	return s, nil
}

// readRetI32 reads a raw i32 from the stack-return area at retptr+offset*4.
func (r *Runtime) readRetI32(retptr uint32, offset int) (uint32, error) {
	mem := r.mod.Memory()
	v, ok := mem.ReadUint32Le(retptr + uint32(offset)*4)
	if !ok {
		return 0, fmt.Errorf("readRetI32: out of bounds read")
	}
	return v, nil
}

// errorFromRef resolves a wasm-bindgen error ref (as stashed in our host ref
// table by whichever import created it) into a Go error.
func (r *Runtime) errorFromRef(ref uint32) error {
	o := r.refTab.resolve(ref)
	if o == nil {
		return fmt.Errorf("wasm error (ref=%d): <no detail>", ref)
	}
	if o.str != "" {
		return fmt.Errorf("wasm error: %s", o.str)
	}
	return fmt.Errorf("wasm error (kind=%d)", o.kind)
}

// GenerateRuntimeAuthFields derives `encrypt_user_info`/`key` runtime fields
// from a minimal user-info JSON payload, mirroring the JS free function
// `eS().generate_runtime_auth_fields(JSON.stringify({uid, organization_id,
// organization_tags, data_policy_agreed}))`. Returns the raw JSON string
// `{"encrypt_user_info":"...","key":"..."}` (whatever shape the WASM emits).
func GenerateRuntimeAuthFields(rt *Runtime, inputJSON string) (string, error) {
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return "", err
	}
	defer rt.addToStackPointer(16)

	ptr, length, err := rt.packString(&inputJSON)
	if err != nil {
		return "", err
	}

	_, err = rt.callWasmFunc("generate_runtime_auth_fields", uint64(retptr), uint64(ptr), uint64(length))
	if err != nil {
		return "", err
	}

	strPtr, err := rt.readRetI32(retptr, 0)
	if err != nil {
		return "", err
	}
	strLen, err := rt.readRetI32(retptr, 1)
	if err != nil {
		return "", err
	}
	errRef, err := rt.readRetI32(retptr, 2)
	if err != nil {
		return "", err
	}
	isErr, err := rt.readRetI32(retptr, 3)
	if err != nil {
		return "", err
	}
	if isErr != 0 {
		return "", rt.errorFromRef(errRef)
	}
	if strPtr == 0 {
		return "", nil
	}
	s := rt.readString(strPtr, strLen)
	rt.free(strPtr, strLen)
	return s, nil
}

// NewContext creates a QoderContext, mirroring the JS constructor:
//
//	new QoderContext(machineId, cosyVersion, userInfoJson, extraConfigJson)
//
// extraConfigJson may be nil (optional in the original JS call site).
func NewContext(rt *Runtime, machineId, cosyVersion, userInfoJson string, extraConfigJson *string) (*Context, error) {
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return nil, err
	}
	defer rt.addToStackPointer(16)

	mPtr, mLen, err := rt.packString(&machineId)
	if err != nil {
		return nil, err
	}
	cvPtr, cvLen, err := rt.packString(&cosyVersion)
	if err != nil {
		return nil, err
	}
	uiPtr, uiLen, err := rt.packString(&userInfoJson)
	if err != nil {
		return nil, err
	}
	ecPtr, ecLen, err := rt.packString(extraConfigJson)
	if err != nil {
		return nil, err
	}

	_, err = rt.callWasmFunc("qodercontext_new",
		uint64(retptr),
		uint64(mPtr), uint64(mLen),
		uint64(cvPtr), uint64(cvLen),
		uint64(uiPtr), uint64(uiLen),
		uint64(ecPtr), uint64(ecLen),
	)
	if err != nil {
		return nil, err
	}

	ctxPtr, err := rt.readRetI32(retptr, 0)
	if err != nil {
		return nil, err
	}
	errRef, err := rt.readRetI32(retptr, 1)
	if err != nil {
		return nil, err
	}
	isErr, err := rt.readRetI32(retptr, 2)
	if err != nil {
		return nil, err
	}
	if isErr != 0 {
		return nil, rt.errorFromRef(errRef)
	}

	return &Context{rt: rt, ptr: ctxPtr}, nil
}

// Free releases the underlying WASM-side QoderContext.
func (c *Context) Free() {
	if c.ptr == 0 {
		return
	}
	_, _ = c.rt.callWasmFunc("__wbg_qodercontext_free", uint64(c.ptr), 0)
	c.ptr = 0
}

// RefreshAuthFields re-derives runtime auth fields from updated user info,
// mirroring `context.refreshAuthFields(userInfoJson)`.
func (c *Context) RefreshAuthFields(userInfoJson string) error {
	rt := c.rt
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return err
	}
	defer rt.addToStackPointer(16)

	uiPtr, uiLen, err := rt.packString(&userInfoJson)
	if err != nil {
		return err
	}

	_, err = rt.callWasmFunc("qodercontext_refreshAuthFields",
		uint64(retptr), uint64(c.ptr), uint64(uiPtr), uint64(uiLen))
	if err != nil {
		return err
	}

	errRef, err := rt.readRetI32(retptr, 0)
	if err != nil {
		return err
	}
	isErr, err := rt.readRetI32(retptr, 1)
	if err != nil {
		return err
	}
	if isErr != 0 {
		return rt.errorFromRef(errRef)
	}
	return nil
}

// PrepareInferRequest signs an agent_chat_generation-style inference request,
// mirroring `context.prepareInferRequest(endpoint, bodyJson, modelKey, source)`.
// modelKey and source may be nil (optional in the original call site).
func (c *Context) PrepareInferRequest(endpoint, bodyJSON string, modelKey, source *string) (*RequestResult, error) {
	rt := c.rt
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return nil, err
	}
	defer rt.addToStackPointer(16)

	ePtr, eLen, err := rt.packString(&endpoint)
	if err != nil {
		return nil, err
	}
	bPtr, bLen, err := rt.packString(&bodyJSON)
	if err != nil {
		return nil, err
	}
	mkPtr, mkLen, err := rt.packString(modelKey)
	if err != nil {
		return nil, err
	}
	srcPtr, srcLen, err := rt.packString(source)
	if err != nil {
		return nil, err
	}

	_, err = rt.callWasmFunc("qodercontext_prepareInferRequest",
		uint64(retptr), uint64(c.ptr),
		uint64(ePtr), uint64(eLen),
		uint64(bPtr), uint64(bLen),
		uint64(mkPtr), uint64(mkLen),
		uint64(srcPtr), uint64(srcLen),
	)
	if err != nil {
		return nil, err
	}

	resPtr, err := rt.readRetI32(retptr, 0)
	if err != nil {
		return nil, err
	}
	errRef, err := rt.readRetI32(retptr, 1)
	if err != nil {
		return nil, err
	}
	isErr, err := rt.readRetI32(retptr, 2)
	if err != nil {
		return nil, err
	}
	if isErr != 0 {
		return nil, rt.errorFromRef(errRef)
	}

	return &RequestResult{rt: rt, ptr: resPtr}, nil
}

// PrepareRequest signs a generic (non-inference) request, mirroring
// `context.prepareRequest(endpoint, path, method, sigPath, body, extra)`.
// body and extra may be nil (optional in the original call site).
func (c *Context) PrepareRequest(endpoint, path, method, sigPath string, body, extra *string) (*RequestResult, error) {
	rt := c.rt
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return nil, err
	}
	defer rt.addToStackPointer(16)

	ePtr, eLen, err := rt.packString(&endpoint)
	if err != nil {
		return nil, err
	}
	pPtr, pLen, err := rt.packString(&path)
	if err != nil {
		return nil, err
	}
	mPtr, mLen, err := rt.packString(&method)
	if err != nil {
		return nil, err
	}
	spPtr, spLen, err := rt.packString(&sigPath)
	if err != nil {
		return nil, err
	}
	bPtr, bLen, err := rt.packString(body)
	if err != nil {
		return nil, err
	}
	xPtr, xLen, err := rt.packString(extra)
	if err != nil {
		return nil, err
	}

	_, err = rt.callWasmFunc("qodercontext_prepareRequest",
		uint64(retptr), uint64(c.ptr),
		uint64(ePtr), uint64(eLen),
		uint64(pPtr), uint64(pLen),
		uint64(mPtr), uint64(mLen),
		uint64(spPtr), uint64(spLen),
		uint64(bPtr), uint64(bLen),
		uint64(xPtr), uint64(xLen),
	)
	if err != nil {
		return nil, err
	}

	resPtr, err := rt.readRetI32(retptr, 0)
	if err != nil {
		return nil, err
	}
	errRef, err := rt.readRetI32(retptr, 1)
	if err != nil {
		return nil, err
	}
	isErr, err := rt.readRetI32(retptr, 2)
	if err != nil {
		return nil, err
	}
	if isErr != 0 {
		return nil, rt.errorFromRef(errRef)
	}

	return &RequestResult{rt: rt, ptr: resPtr}, nil
}

// Free releases the underlying WASM-side RequestResult.
func (rr *RequestResult) Free() {
	if rr.ptr == 0 {
		return
	}
	_, _ = rr.rt.callWasmFunc("__wbg_requestresult_free", uint64(rr.ptr), 0)
	rr.ptr = 0
}

// URL returns the signed request URL.
func (rr *RequestResult) URL() (string, error) {
	rt := rr.rt
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return "", err
	}
	defer rt.addToStackPointer(16)

	_, err = rt.callWasmFunc("requestresult_url", uint64(retptr), uint64(rr.ptr))
	if err != nil {
		return "", err
	}
	return rt.readRetString(retptr, 0)
}

// Body returns the (JSON, UTF-8 text) request body, if any.
func (rr *RequestResult) Body() (string, error) {
	rt := rr.rt
	retptr, err := rt.addToStackPointer(-16)
	if err != nil {
		return "", err
	}
	defer rt.addToStackPointer(16)

	_, err = rt.callWasmFunc("requestresult_body", uint64(retptr), uint64(rr.ptr))
	if err != nil {
		return "", err
	}
	return rt.readRetString(retptr, 0)
}

// Headers returns the signed request headers as a plain map, resolving the
// Map ref that requestresult_headers hands back (mirrors JS `fa(result.headers)`).
func (rr *RequestResult) Headers() (map[string]string, error) {
	rt := rr.rt
	results, err := rt.callWasmFunc("requestresult_headers", uint64(rr.ptr))
	if err != nil {
		return nil, err
	}
	ref := uint32(results[0])
	o := rt.refTab.resolve(ref)
	headers := make(map[string]string)
	if o == nil || o.members == nil {
		return headers, nil
	}
	for k, v := range o.members {
		if v == nil {
			continue
		}
		headers[k] = v.str
	}
	return headers, nil
}
