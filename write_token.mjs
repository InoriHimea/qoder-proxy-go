import fs from 'fs';
import path from 'path';
import os from 'os';
import { fileURLToPath } from 'url';
import { execSync } from 'child_process';

const backend = process.argv[2] || 'cn'; // 'cn' or 'global'
const token = process.argv[3];
const userId = process.argv[4] || '';
const refreshToken = process.argv[5] || '';
const expireTime = process.argv[6] ? parseInt(process.argv[6], 10) : 2000000000;

if (!token) {
    console.error('Usage: node write_token.mjs <backend> <token> [userId] [refreshToken] [expireTime]');
    process.exit(1);
}

// Find qodercli package bundle path
const pkgName = backend === 'cn' ? '@qodercn-ai/qoderclicn' : '@qoder-ai/qodercli';
const bundleFile = backend === 'cn' ? 'qoderclicn.js' : 'qodercli.js';

const searchPaths = [];

try {
    const globalRoot = execSync('npm root -g').toString().trim();
    if (globalRoot) {
        searchPaths.push(path.join(globalRoot, pkgName, 'bundle', bundleFile));
    }
} catch (e) {
    // Ignore error if npm is not found
}

const homeDir = os.homedir();

if (process.platform === 'win32') {
    const appData = process.env.APPDATA || path.join(homeDir, 'AppData', 'Roaming');
    searchPaths.push(path.join(appData, 'npm', 'node_modules', pkgName, 'bundle', bundleFile));
} else {
    searchPaths.push(
        path.join('/usr/lib/node_modules', pkgName, 'bundle', bundleFile),
        path.join('/usr/local/lib/node_modules', pkgName, 'bundle', bundleFile),
        path.join(homeDir, '.npm-global', 'lib', 'node_modules', pkgName, 'bundle', bundleFile),
        path.join(homeDir, '.config', 'yarn', 'global', 'node_modules', pkgName, 'bundle', bundleFile)
    );
}

let foundPath = null;
for (const p of searchPaths) {
    if (fs.existsSync(p)) {
        foundPath = p;
        break;
    }
}

if (!foundPath) {
    console.error(`Could not find ${bundleFile} in search paths:`, searchPaths);
    process.exit(1);
}

console.log(`Found bundle at: ${foundPath}`);

try {
    let code = fs.readFileSync(foundPath, 'utf8');
    
    // Remove self execution
    const tailCode = code.substring(Math.max(0, code.length - 5000));
    const selfExecRegex = /(?:[a-zA-Z0-9_$]+\([^()]*\),)+[a-zA-Z0-9_$]+\(\)\.catch/g;
    const matches = [...tailCode.matchAll(selfExecRegex)];
    if (matches.length === 0) {
        throw new Error('Self-execution pattern not found in bundle');
    }
    const lastMatch = matches[matches.length - 1];
    const index = Math.max(0, code.length - 5000) + lastMatch.index;
    const selfExecPattern = lastMatch[0];
    console.log(`Found self-execution pattern: "${selfExecPattern}" at index ${index}`);
    
    // Detect symbol names dynamically
    const wasmInitRegex = /async\s+function\s+([a-zA-Z0-9_$]+)\(\)\{\s*return\s+[a-zA-Z0-9_$]+\|\|[a-zA-Z0-9_$]+\|\|\([a-zA-Z0-9_$]+=\(async\(\)=>{try\{let\s+[a-zA-Z0-9_$]+=await\s+[a-zA-Z0-9_$]+\(\)/g;
    const wasmMatches = [...code.matchAll(wasmInitRegex)];
    if (wasmMatches.length === 0) {
        throw new Error('Could not dynamically find WASM init function');
    }
    const wasmInitName = wasmMatches[0][1];

    let credClassName = null;
    const saveIdx = code.indexOf('static async save(');
    if (saveIdx !== -1) {
        const prefix = code.substring(saveIdx - 50, saveIdx);
        const m = prefix.match(/([a-zA-Z0-9_$]+)=class [a-zA-Z0-9_$]+\{$/);
        if (m) credClassName = m[1];
    }
    if (!credClassName) {
        throw new Error('Could not dynamically find Credential Storage Class');
    }

    let pathFnName = null;
    const userStrIdx = code.indexOf(',"user")');
    if (userStrIdx !== -1) {
        const prefix = code.substring(Math.max(0, userStrIdx - 150), userStrIdx);
        const m = prefix.match(/function ([a-zA-Z0-9_$]+)\(\)\{/);
        if (m) pathFnName = m[1];
    }
    if (!pathFnName) {
        throw new Error('Could not dynamically find User Path Function');
    }

    // The credential class and path function live inside an esbuild lazy-init
    // wrapper (`NAME=HELPER(()=>{...})`) that populates module-level state (base
    // dirs, endpoint config, etc). Importing the trimmed bundle skips whatever
    // normally triggers that wrapper, so we detect its name and invoke it
    // ourselves before touching credClass/pathFn. The minified helper name
    // differs across CLI releases (M in 1.0.37, p in 1.1.5), so match it too.
    let lazyInitName = null;
    if (saveIdx !== -1) {
        const before = code.substring(Math.max(0, saveIdx - 3000), saveIdx);
        const wraps = [...before.matchAll(/([a-zA-Z0-9_$]+)=([a-zA-Z0-9_$]{1,3})\(\(\)=>\{/g)];
        if (wraps.length > 0) lazyInitName = wraps[wraps.length - 1][1];
    }
    if (!lazyInitName) {
        throw new Error('Could not dynamically find lazy-init wrapper for Credential Storage Class');
    }

    console.log(`Dynamic Symbols Detected: wasmInit="${wasmInitName}", credClass="${credClassName}", pathFn="${pathFnName}", lazyInit="${lazyInitName}"`);

    // Create temp file in system tmp directory with exports appended
    const tempFile = path.join(os.tmpdir(), `qodercli_temp_${Date.now()}.mjs`);
    const tempCode = code.substring(0, index) + `\nexport { ${wasmInitName} as HT, ${credClassName} as fc, ${pathFnName} as eee, ${lazyInitName} as lzi };\n`;

    fs.writeFileSync(tempFile, tempCode, 'utf8');
    console.log(`Created temp wrapper module at: ${tempFile}`);

    // Convert path to file:/// URL for dynamic import on Windows
    const tempFileUri = `file:///${tempFile.replace(/\\/g, '/')}`;
    
    console.log('Importing temp wrapper...');
    // NOTE: must use the namespace object and read properties AFTER calling
    // lzi(), not destructure up front — destructuring snapshots the property
    // value at that instant, and fc/eee are still undefined until the
    // lazy-init wrapper (lzi) actually runs and assigns them.
    const ns = await import(tempFileUri);
    ns.lzi(); // trigger lazy-init wrapper so module-level state (base dirs, etc) is populated
    const { HT, fc, eee } = ns;
    
    // Exchange PAT for DT if needed
    let finalToken = token;
    if (token.startsWith('pt-') || token.startsWith('qodercn-')) {
        console.log('Detected Personal Access Token, exchanging for device token...');
        const baseURL = backend === 'cn' ? 'https://openapi.qoder.com.cn/api/v1' : 'https://openapi.qoder.sh/api/v1';
        try {
            const res = await fetch(`${baseURL}/jobToken/exchange`, {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    'User-Agent': 'qoder/1.0.22',
                    'Cosy-Version': '1.0.22',
                    'Cosy-ClientType': '5',
                    'Cosy-MachineOS': process.platform === 'win32' ? 'x86_64_win32' : 'x86_64_linux'
                },
                body: JSON.stringify({ personal_token: token })
            });
            if (res.ok) {
                const data = await res.json();
                if (data.token || data.device_token || data.access_token) {
                    finalToken = data.token || data.device_token || data.access_token;
                    console.log('Successfully exchanged token!');
                } else {
                    console.error('Failed to parse token from exchange response:', data);
                }
            } else {
                console.error('Failed to exchange token. Status:', res.status, await res.text());
            }
        } catch (err) {
            console.error('Error exchanging token:', err);
        }
    }

    console.log('Initializing WASM...');
    await HT();
    
    const credentials = {
        uid: userId || "019ece1a-7036-75f3-be32-145c882786df",
        name: "qoder_user",
        security_oauth_token: finalToken,
        access_token: finalToken,
        refresh_token: refreshToken || "drt-dummy",
        expire_time: expireTime,
        refresh_token_expire_time: expireTime,
        login_method: "browser",
        login_timestamp: Math.floor(Date.now() / 1000),
        data_policy_agreed: true
    };
    
    console.log('Saving credentials to:', eee());
    await fc.save(credentials);
    console.log('Token successfully written to local storage!');
    
    // Clean up temp file
    try {
        fs.unlinkSync(tempFile);
    } catch {}
    
    process.exit(0);
} catch (e) {
    console.error('Error during token write execution:', e);
    process.exit(1);
}
