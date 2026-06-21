import fs from 'fs';
import path from 'path';
import os from 'os';
import { fileURLToPath } from 'url';
import { execSync } from 'child_process';

const backend = process.argv[2] || 'cn'; // 'cn' or 'global'
const token = process.argv[3];

if (!token) {
    console.error('Usage: node write_token.mjs <backend> <token>');
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
    const selfExecRegex = /[a-zA-Z0-9_$]+\(\),[a-zA-Z0-9_$]+\(\),[a-zA-Z0-9_$]+\(\)\.catch/g;
    const matches = [...code.matchAll(selfExecRegex)];
    if (matches.length === 0) {
        throw new Error('Self-execution pattern not found in bundle');
    }
    const lastMatch = matches[matches.length - 1];
    const index = lastMatch.index;
    const selfExecPattern = lastMatch[0];
    console.log(`Found self-execution pattern: "${selfExecPattern}" at index ${index}`);
    
    // Detect symbol names dynamically
    const wasmInitRegex = /async\s+function\s+([a-zA-Z0-9_$]+)\(\)\{\s*return\s+[a-zA-Z0-9_$]+\|\|[a-zA-Z0-9_$]+\|\|\([a-zA-Z0-9_$]+=\(async\(\)=>{try\{let\s+[a-zA-Z0-9_$]+=await\s+[a-zA-Z0-9_$]+\(\)/g;
    const wasmMatches = [...code.matchAll(wasmInitRegex)];
    if (wasmMatches.length === 0) {
        throw new Error('Could not dynamically find WASM init function');
    }
    const wasmInitName = wasmMatches[0][1];

    const credClassRegex = /([a-zA-Z0-9_$]+)\s*=\s*class\s+A\s*\{\s*static\s+async\s+save\(/g;
    const credMatches = [...code.matchAll(credClassRegex)];
    if (credMatches.length === 0) {
        throw new Error('Could not dynamically find Credential Storage Class');
    }
    const credClassName = credMatches[0][1];

    const pathFnRegex = /function\s+([a-zA-Z0-9_$]+)\(\)\{\s*let\s+[a-zA-Z0-9_$]+\s*=\s*[a-zA-Z0-9_$]+\(\);\s*return\s*"prod"\s*===\s*[a-zA-Z0-9_$]+\s*\?\s*[a-zA-Z0-9_$]+\.join\(\s*[a-zA-Z0-9_$]+,\s*["']user["']\)\s*:\s*[a-zA-Z0-9_$]+\.join\(\s*[a-zA-Z0-9_$]+,\s*[\`"']user\.\$\{[a-zA-Z0-9_$]+\}[\`"']\)\}/g;
    const pathMatches = [...code.matchAll(pathFnRegex)];
    if (pathMatches.length === 0) {
        throw new Error('Could not dynamically find User Path Function');
    }
    const pathFnName = pathMatches[0][1];

    console.log(`Dynamic Symbols Detected: wasmInit="${wasmInitName}", credClass="${credClassName}", pathFn="${pathFnName}"`);

    // Create temp file in system tmp directory with exports appended
    const tempFile = path.join(os.tmpdir(), `qodercli_temp_${Date.now()}.mjs`);
    const tempCode = code.substring(0, index) + `\nexport { ${wasmInitName} as HT, ${credClassName} as fc, ${pathFnName} as eee };\n`;
    
    fs.writeFileSync(tempFile, tempCode, 'utf8');
    console.log(`Created temp wrapper module at: ${tempFile}`);
    
    // Convert path to file:/// URL for dynamic import on Windows
    const tempFileUri = `file:///${tempFile.replace(/\\/g, '/')}`;
    
    console.log('Importing temp wrapper...');
    const { HT, fc, eee } = await import(tempFileUri);
    
    console.log('Initializing WASM...');
    await HT();
    
    const credentials = {
        uid: "019ece1a-7036-75f3-be32-145c882786df",
        name: "qoder_user",
        security_oauth_token: token,
        access_token: token,
        refresh_token: "drt-dummy",
        expire_time: 2000000000,
        refresh_token_expire_time: 2000000000,
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
