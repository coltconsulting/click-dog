#!/usr/bin/env bash
set -euo pipefail

qualifier="${1:-}"
explicit_version="${2:-}"
year="${3:-$(date +%y)}"
month="${4:-$(date +%m)}"

# Every index and ordinal below comes from `git tag -l`. Outside a repository
# that reads as zero tags rather than as an error — `git tag -l` writes its
# fatal to stderr inside a process substitution, where set -e cannot see it —
# so the script would confidently print the first tag of the month regardless
# of how many releases exist. Refuse instead. Callers that must work in an
# extracted archive (the Makefile's VERSION) check for a repository first.
git rev-parse --git-dir >/dev/null 2>&1 || {
	echo "error: not a git repository — the next tag is computed from the existing tags" >&2
	exit 2
}

# Three channels. alpha is the default because it is the everyday one: it never
# leaves the internal repository. beta and ga both reach the public repository,
# so both must be asked for by name.
case "$qualifier" in
	""|alpha|beta|ga) ;;
	*)
		echo "error: invalid qualifier '$qualifier' — must be one of: alpha, beta, ga" >&2
		exit 2
		;;
esac
[ -n "$qualifier" ] || qualifier=alpha

if [[ ! "$year" =~ ^[0-9]{2}$ ]] || [[ ! "$month" =~ ^(0[1-9]|1[0-2])$ ]]; then
	echo "error: year/month must use YY MM (got '$year' '$month')" >&2
	exit 2
fi

if [ -n "$explicit_version" ]; then
	if [[ ! "$explicit_version" =~ ^[0-9]{2}\.(0[1-9]|1[0-2])\.[1-9][0-9]*$ ]]; then
		echo "error: invalid version '$explicit_version' — expected YY.MM.idx (for example 26.08.3)" >&2
		exit 2
	fi
	base="v$explicit_version"
else
	max_ga=0
	while IFS= read -r tag; do
		if [[ "$tag" =~ ^v${year}\.${month}\.([1-9][0-9]*)$ ]]; then
			idx="${BASH_REMATCH[1]}"
			if (( idx > max_ga )); then
				max_ga="$idx"
			fi
		fi
	done < <(git tag -l "v${year}.${month}.*")
	base="v${year}.${month}.$((max_ga + 1))"
fi

if [ "$qualifier" = "ga" ]; then
	printf '%s\n' "$base"
	exit 0
fi

max_ordinal=0
while IFS= read -r tag; do
	if [[ "$tag" =~ ^${base}-${qualifier}\.([1-9][0-9]*)$ ]]; then
		ordinal="${BASH_REMATCH[1]}"
		if (( ordinal > max_ordinal )); then
			max_ordinal="$ordinal"
		fi
	fi
done < <(git tag -l "${base}-${qualifier}.*")

printf '%s-%s.%d\n' "$base" "$qualifier" "$((max_ordinal + 1))"
