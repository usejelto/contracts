#!/usr/bin/env python3
"""Compare golangci-lint or Knip findings with a reviewed baseline.

    lint_baseline.py check <report-json> <baseline>   fail on any difference
    lint_baseline.py write <report-json> <baseline>   rewrite the baseline

The report is golangci-lint's JSON output or Knip's (`--reporter json`).

A baseline records the findings that already existed when a linter was
adopted, so that only a new one fails. Each entry is a path, a linter, the
message and how many times that combination occurs; line numbers are left out
so that unrelated edits do not churn it. `check` fails on a finding outside
the baseline, and also on a baseline entry that no longer occurs, so a fix
shrinks the file in the same change instead of leaving room for the finding
to return unnoticed. `write` is a deliberate act whose diff is reviewed.

Standard library only.
"""

import json
import sys
from collections import Counter
from pathlib import Path


def knip_findings(data):
    """Knip reports issues per file and category; each item is one finding."""
    for issue in data.get('issues') or []:
        for category, items in issue.items():
            if not isinstance(items, list):
                continue
            for item in items:
                if isinstance(item, list):
                    name = '|'.join(part['name'] for part in item)
                    line = item[0].get('line', 0) if item else 0
                else:
                    name, line = item.get('name', ''), item.get('line', 0)
                yield {'FromLinter': f'knip:{category}', 'Text': name,
                       'Pos': {'Filename': issue['file'], 'Line': line}}


def findings(report_path):
    data = json.loads(Path(report_path).read_text() or '{}')
    root = Path.cwd().resolve()
    out = Counter()
    where = {}
    issues = data.get('Issues') if 'Issues' in data else list(knip_findings(data))
    for issue in issues or []:
        name = Path(issue['Pos']['Filename'])
        if name.is_absolute():
            name = name.resolve().relative_to(root)
        else:
            name = (root / name).resolve().relative_to(root)
        key = (name.as_posix(), issue['FromLinter'], issue['Text'])
        out[key] += 1
        where.setdefault(key, []).append(issue['Pos']['Line'])
    return out, where


def load(baseline_path):
    path = Path(baseline_path)
    if not path.exists():
        return Counter()
    data = json.loads(path.read_text())
    return Counter({(e['path'], e['linter'], e['text']): e['count'] for e in data['findings']})


def write(found, baseline_path):
    entries = [{'path': p, 'linter': l, 'text': t, 'count': n} for (p, l, t), n in sorted(found.items())]
    Path(baseline_path).write_text(json.dumps({
        '_comment': ('Findings that predate the linter, reviewed when it was adopted. '
                     'Fix them and shrink this file; never add to it to make a check pass. '
                     'Rewritten by lint_baseline.py write.'),
        'findings': entries,
    }, indent=2) + '\n')
    print(f'{baseline_path}: {sum(found.values())} findings in {len(entries)} entries')


def check(found, where, baseline):
    new = found - baseline
    gone = baseline - found
    for (path, linter, text), n in sorted(new.items()):
        lines = ', '.join(str(l) for l in sorted(where[(path, linter, text)]))
        print(f'{path}:{lines}: {text} ({linter}){"" if n == 1 else f" x{n} new"}', file=sys.stderr)
    for (path, linter, text), n in sorted(gone.items()):
        print(f'{path}: fixed, still in the baseline: {text} ({linter}) x{n}', file=sys.stderr)
    kept = sum((found & baseline).values())
    if new or gone:
        if gone:
            print('lint_baseline: remove the fixed entries with `make lint-baseline` and review the diff',
                  file=sys.stderr)
        print(f'lint_baseline: {sum(new.values())} new, {sum(gone.values())} fixed but listed, '
              f'{kept} known', file=sys.stderr)
        return 1
    print(f'lint_baseline: no new findings ({kept} known, listed in the baseline)')
    return 0


def main(argv):
    if len(argv) != 3 or argv[0] not in {'check', 'write'}:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    command, report, baseline = argv
    found, where = findings(report)
    if command == 'write':
        write(found, baseline)
        return 0
    return check(found, where, load(baseline))


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
