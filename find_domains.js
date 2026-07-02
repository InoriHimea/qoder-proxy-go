const fs=require('fs');
let code=fs.readFileSync('C:\\Users\\25321\\AppData\\Roaming\\npm\\node_modules\\@qodercn-ai\\qoderclicn\\bundle\\qoderclicn.js', 'utf8');
let matches = [...code.matchAll(/https:\/\/[^\/\"\'\`\\]+/g)];
let domains = new Set(matches.map(m=>m[0]));
console.log(Array.from(domains));
