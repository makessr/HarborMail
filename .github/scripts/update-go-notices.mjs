import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const platforms = ['linux/amd64', 'linux/arm64', 'windows/amd64', 'windows/arm64'];
const modules = new Map();
const go = (args, env = process.env) => execFileSync('go', args, { encoding: 'utf8', env }).trim();
const moduleFormat = '{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}';

for (const platform of platforms) {
  const [GOOS, GOARCH] = platform.split('/');
  const dependencies = go(['list', '-buildvcs=false', '-deps', '-f', moduleFormat, './cmd/harbormail'], {
    ...process.env, CGO_ENABLED: '0', GOOS, GOARCH,
  });
  for (const line of dependencies.split(/\r?\n/).filter(Boolean)) {
    const [name, version, directory] = line.split('|');
    modules.set(name, { name, version, directory });
  }
}

const notice = ['Harbor Mail Go runtime notices', `Targets: ${platforms.join(', ')}.`];
const readText = file => readFileSync(file, 'utf8').replace(/\r\n/g, '\n');

function appendNotice(title, directory, files, source) {
  if (files.length === 0) throw new Error(`No upstream license found for ${title}`);
  notice.push('', '='.repeat(72), title, `Source: ${source}`);
  for (const file of files) {
    const text = readText(join(directory, file)).replace(/[ \t]+$/gm, '').trimEnd();
    notice.push('', `--- ${file} ---`, text);
  }
}

const version = go(['env', 'GOVERSION']);
appendNotice(`Go standard library ${version}`, go(['env', 'GOROOT']), ['LICENSE', 'PATENTS'],
  `https://go.dev/dl/${version}.src.tar.gz`);

for (const { name, version, directory } of [...modules.values()].sort((a, b) => a.name.localeCompare(b.name, 'en'))) {
  const path = name.replace(/[A-Z]/g, letter => `!${letter.toLowerCase()}`);
  const files = readdirSync(directory).filter(file => /^(LICENSE|COPYING|COPYRIGHT|NOTICE|PATENTS)(?:[.-].+)?$/i.test(file));
  appendNotice(`${name} ${version}`, directory, files, `https://proxy.golang.org/${path}/@v/${version}.zip`);
}

const checksums = ['go.mod', 'go.sum'].map(file => {
  const checksum = createHash('sha256').update(readText(file)).digest('hex');
  return `${checksum}  ${file}`;
});
writeFileSync('docs/licenses/go-runtime.txt', `${notice.join('\n')}\n`);
writeFileSync('docs/licenses/go-inputs.sha256', `${checksums.join('\n')}\n`);
