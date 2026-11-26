import argparse
import collections
import ipaddress
import json
from pathlib import Path
import re
import subprocess
import tempfile


def load_allowlist(path):
    entries = json.loads(path.read_text())
    if not isinstance(entries, list) or len(entries) != 107:
        raise ValueError('The reviewed allowlist must contain exactly 107 entries.')
    allowed = {}
    reasons = collections.Counter()
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {'blob', 'reason'}:
            raise ValueError('Each allowlist entry must contain only blob and reason.')
        blob, reason = entry['blob'], entry['reason']
        if not isinstance(blob, str) or not re.fullmatch(r'[0-9a-f]{40}', blob):
            raise ValueError('Each allowlist blob must be a full lowercase SHA-1.')
        if reason not in ('upstream-provenance', 'false-positive section anchor'):
            raise ValueError('Each allowlist entry needs a reviewed reason.')
        if blob in allowed:
            raise ValueError('Duplicate allowlist blob.')
        allowed[blob] = reason
        reasons[reason] += 1
    if reasons != {'upstream-provenance': 106, 'false-positive section anchor': 1}:
        raise ValueError('The reviewed reason counts must remain 106 and 1.')
    return allowed


def scan(ref, allowed):
    if subprocess.check_output(['git', 'rev-parse', '--is-shallow-repository']).strip() != b'false':
        raise RuntimeError('Full-history scanning requires a non-shallow repository.')
    ref = subprocess.check_output(
        ['git', 'rev-parse', '--verify', '--end-of-options', ref + '^{commit}'], text=True,
    ).strip()
    listed = subprocess.check_output(['git', 'rev-list', '--objects', ref])
    objects = {}
    for row in listed.splitlines():
        oid, _, path = row.partition(b' ')
        objects[oid] = path.decode('utf8', 'replace')
    checks = subprocess.check_output(
        ['git', 'cat-file', '--batch-check=%(objectname) %(objecttype) %(objectsize)'],
        input=b'\n'.join(objects) + b'\n',
    )
    selected = [(oid, kind, int(size)) for oid, kind, size in
                (row.split() for row in checks.splitlines()) if kind in (b'blob', b'commit')]
    patterns = {
        'banned-host': re.compile(rb'par' + rb'4|home[.]neonbots[.]dev', re.I),
        'internal-absolute-path': re.compile(rb'/(?:mnt/user|home/(?:agent|jeremy)|var/lib/docker)/', re.I),
    }
    ip_pattern = re.compile(rb'(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])')
    counts = collections.Counter()
    unapproved = collections.Counter()
    examples = collections.defaultdict(list)
    approved = set()
    byte_count = 0
    with tempfile.TemporaryFile() as requests:
        requests.write(b'\n'.join(oid for oid, _, _ in selected) + b'\n')
        requests.seek(0)
        process = subprocess.Popen(['git', 'cat-file', '--batch'], stdin=requests, stdout=subprocess.PIPE)
        try:
            for oid, kind, size in selected:
                header = process.stdout.readline().split()
                if header != [oid, kind, str(size).encode()]:
                    raise RuntimeError('Unexpected Git object header.')
                body = process.stdout.read(size)
                if len(body) != size or process.stdout.read(1) != b'\n':
                    raise RuntimeError('Incomplete Git object.')
                byte_count += size
                categories = [name for name, pattern in patterns.items() if pattern.search(body)]
                if kind == b'blob' and b'\0' not in body[:8192]:
                    for match in ip_pattern.finditer(body):
                        try:
                            address = ipaddress.ip_address(match[0].decode())
                        except ValueError:
                            continue
                        if address.is_private and not address.is_loopback and not address.is_unspecified:
                            categories.append('private-IP-literal')
                            break
                for category in categories:
                    counts[category] += 1
                    if category == 'private-IP-literal' and oid.decode() in allowed:
                        approved.add(oid.decode())
                        continue
                    unapproved[category] += 1
                    if len(examples[category]) < 8:
                        examples[category].append({'object': oid.decode(), 'path': objects[oid]})
        finally:
            process.stdout.close()
            status = process.wait()
        if status:
            raise RuntimeError('Git object reader failed.')
    unused = sorted(set(allowed) - approved)
    return {
        'ref': ref,
        'commit': ref,
        'objects_scanned': len(selected),
        'bytes_scanned': byte_count,
        'matching_objects': dict(counts),
        'allowlisted_objects': len(approved),
        'allowlist_reasons': dict(collections.Counter(allowed[oid] for oid in approved)),
        'unused_allowlist_entries': unused,
        'unapproved_matching_objects': dict(unapproved),
        'examples': dict(examples),
        'gate': 'STOP' if unapproved or unused else 'PASS',
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('ref', nargs='?', default='HEAD')
    parser.add_argument('--allowlist', type=Path, default=Path(__file__).with_name('public-history-allowlist.json'))
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    try:
        allowed = load_allowlist(args.allowlist)
    except (OSError, ValueError) as error:
        parser.error(str(error))
    report = scan(args.ref, allowed)
    report['ref'] = args.ref
    encoded = json.dumps(report, indent=2) + '\n'
    if args.output:
        args.output.write_text(encoded)
    print(encoded, end='')
    return 1 if report['gate'] == 'STOP' else 0


if __name__ == '__main__':
    raise SystemExit(main())
