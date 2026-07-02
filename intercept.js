const https = require('https');
const http = require('http');

function hookReq(mod, name) {
    const orig = mod.request;
    mod.request = function(...args) {
        let opts = args[0];
        if (typeof opts === 'string') {
            console.error(`[INTERCEPT ${name}] URL:`, opts);
        } else if (opts && opts.hostname) {
            console.error(`[INTERCEPT ${name}] HOST:`, opts.hostname, 'PATH:', opts.path, 'HEADERS:', opts.headers);
        } else {
            console.error(`[INTERCEPT ${name}] OPTS:`, opts);
        }
        
        const req = orig.apply(this, args);
        
        const origWrite = req.write;
        req.write = function(chunk) {
            console.error(`[INTERCEPT ${name} BODY]:`, chunk ? chunk.toString() : '');
            return origWrite.apply(this, arguments);
        };
        
        const origEnd = req.end;
        req.end = function(chunk) {
            if (chunk && typeof chunk !== 'function') console.error(`[INTERCEPT ${name} END BODY]:`, chunk.toString());
            return origEnd.apply(this, arguments);
        };
        
        req.on('response', (res) => {
            console.error(`[INTERCEPT ${name} RES]:`, res.statusCode);
            res.on('data', d => console.error(`[INTERCEPT ${name} RES BODY]:`, d.toString()));
        });
        
        return req;
    };
}

hookReq(https, 'HTTPS');
hookReq(http, 'HTTP');

if (typeof global.fetch === 'function') {
    const origFetch = global.fetch;
    global.fetch = async function(...args) {
        console.error('[INTERCEPT FETCH]:', args);
        return origFetch.apply(this, args);
    };
}

const bundlePath = 'C:\\\\Users\\\\25321\\\\AppData\\\\Roaming\\\\npm\\\\node_modules\\\\@qodercn-ai\\\\qoderclicn\\\\bundle\\\\qoderclicn.js';
process.argv = ['node', bundlePath, 'chat', '-m', 'auto', 'hello'];
require(bundlePath);
