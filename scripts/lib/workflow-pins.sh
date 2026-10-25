#!/usr/bin/env bash
# Fail when a GitHub workflow stops reading its tool versions from
# scripts/tool-versions.env.
#
# Every workflow takes its pinned versions from the pin file, either by
# sourcing it inline or from a "Read tool pins" step (id: pins) that writes
# them to $GITHUB_OUTPUT for later steps to read as steps.pins.outputs.<name>.
# Two mistakes survive actionlint: a version literal copied back into a
# workflow, which a later bump to the pin file leaves behind, and a mistyped
# output name such as steps.pins.outputs.golangci_version, which actionlint
# cannot see because a run script writes the outputs. In CI the mistyped name
# is an empty string, and setup-go or golangci-lint-action then picks a
# version of its own.
#
#   scripts/lib/workflow-pins.sh [root]
#
# checks the tree at <root> (default: this checkout), names every problem, and
# exits 1 when
#   - a workflow contains the value of any *_VERSION pin as a version token
#     (1.26.9 matches "go1.26.9" and "v1.26.9", but not 1.26.90 or 11.26.9),
#     outside a `uses:` line, whose ref and "# vX.Y.Z" comment Dependabot
#     rewrites to whatever version the action has reached;
#   - an actions/setup-go step's go-version, or a golangci-lint-action step's
#     version, is anything but a ${{ steps.pins.outputs.<name> }} read whose
#     value is the GO_VERSION or GOLANGCI_LINT_VERSION pin: a stale literal
#     such as '1.26.9', '1.26' or stable names no current pin, so only this
#     catches it;
#   - a job reads steps.pins.outputs.<name> that its pins step does not write,
#     writes empty, or has not yet run by the step that reads it;
#   - a pins step sets shell: or working-directory:, or a defaults.run over it
#     does;
#   - go.mod's toolchain line is not go$GO_VERSION.
# Each pins step's run script is run with bash from <root>, in an otherwise
# empty environment, against a scratch $GITHUB_OUTPUT, so what it writes is
# what CI would write. That is how CI runs it only while the step keeps the
# default shell and working directory, hence the refusal above. Only
# name=value output lines are read.

# The parser for one workflow, in awk because the shell suites carry no YAML
# library. It understands the subset these workflows use: a top-level `jobs:`
# mapping, each job's `steps:` list, block scalars (whose content is never
# structure), and full-line comments. For each workflow it prints
# tab-separated records
#   pins <job> <step> <line> <script>   the job's id: pins step; its run
#                                       script is written to <script>
#   pins-expr <job> <step> <line> <script>
#                                       the same, for a script that holds a
#                                       ${{ }} expression only CI can evaluate
#   ref  <job> <step> <line> <name>     a read of steps.pins.outputs.<name>;
#                                       step 0 is outside the steps list (job
#                                       outputs, read after every step)
#   bad  <job> <step> <line> -          a steps.pins.outputs read with no
#                                       .<name> after it
#   tool <job> <step> <line> <pin> <key> <value>
#                                       a setup-go or golangci-lint-action
#                                       step, the input that sets its version,
#                                       and that input's value (empty when the
#                                       step has none; <line> is then the
#                                       step's)
#   env  <job> <step> <line> - <key>    a pins step's shell: or
#                                       working-directory:
#   defaults <job> 0 <line> -           a defaults.run; <job> is * for the
#                                       workflow's own
PARSER=""
IFS= read -r -d '' PARSER <<'AWK' || true
function lead(s) {
	match(s, /^ */)
	return RLENGTH
}
function unquote(v) {
	sub(/^ +/, "", v)
	sub(/ +#.*$/, "", v)
	sub(/ +$/, "", v)
	if (v ~ /^".*"$/ || v ~ /^'.*'$/)
		v = substr(v, 2, length(v) - 2)
	return v
}
function tool(pin, key) {
	print "tool\t" job "\t" step "\t" (input_line[key] ? input_line[key] : step_line) "\t" pin "\t" key "\t" input[key]
}
function end_step(   path) {
	if (step > 0 && step_pins) {
		path = prefix job "." step ".sh"
		printf "%s", step_run > path
		close(path)
		print (index(step_run, "${{") ? "pins-expr" : "pins") "\t" job "\t" step "\t" step_line "\t" path
		if (env_key != "")
			print "env\t" job "\t" step "\t" env_line "\t-\t" env_key
	}
	if (step > 0 && step_uses ~ /^actions\/setup-go@/)
		tool("GO_VERSION", "go-version")
	if (step > 0 && step_uses ~ /^golangci\/golangci-lint-action@/)
		tool("GOLANGCI_LINT_VERSION", "version")
	step_pins = 0
	step_run = ""
	step_col = -1
	step_uses = ""
	env_key = ""
	in_with = 0
	split("", input)
	split("", input_line)
}
# defaults(<job>, <n>, <text>): a defaults: mapping's run: child is a
# defaults.run. The first line under defaults: sets its children's indent.
function defaults(who, n, text) {
	if (defaults_col < 0)
		defaults_col = n
	if (n == defaults_col && text ~ /^run:( |$)/)
		print "defaults\t" who "\t0\t" NR "\t-"
}
function scan(s,   rest) {
	while (match(s, /steps\.pins\.outputs/)) {
		rest = substr(s, RSTART + RLENGTH)
		if (match(rest, /^\.[A-Za-z_][A-Za-z0-9_-]*/))
			print "ref\t" job "\t" (in_steps ? step : 0) "\t" NR "\t" substr(rest, 2, RLENGTH - 1)
		else
			print "bad\t" job "\t" (in_steps ? step : 0) "\t" NR "\t-"
		s = rest
	}
}
BEGIN {
	job_indent = -1
	step_col = -1
}
{
	sub(/\r$/, "")
	n = ($0 ~ /^ *$/) ? -1 : lead($0)

	# A block scalar runs until a line no deeper than the key that opened it.
	# Its expressions are still evaluated, so its reads still count.
	if (in_block) {
		if (n < 0 || n > block_key) {
			if (job != "")
				scan($0)
			if (capture) {
				if (n >= 0 && content < 0)
					content = n
				step_run = step_run (n < 0 ? "" : substr($0, content + 1)) "\n"
			}
			next
		}
		in_block = 0
		capture = 0
	}
	if (n < 0 || substr($0, n + 1, 1) == "#")
		next

	if (n == 0) {
		end_step()
		in_jobs = ($0 ~ /^jobs: *(#.*)?$/)
		in_top_defaults = ($0 ~ /^defaults: *(#.*)?$/)
		defaults_col = -1
		job = ""
		job_indent = -1
		in_steps = 0
		next
	}
	if (in_top_defaults)
		defaults("*", n, substr($0, n + 1))
	if (!in_jobs)
		next
	if (job_indent < 0)
		job_indent = n
	if (n <= job_indent) {
		end_step()
		job = ""
		body = -1
		in_steps = 0
		in_defaults = 0
		step = 0
		if (n == job_indent && match($0, /^ *[A-Za-z_][A-Za-z0-9_-]*:/))
			job = substr($0, n + 1, RLENGTH - n - 1)
		next
	}
	if (job == "")
		next
	if (body < 0)
		body = n

	text = substr($0, n + 1)
	col = n
	if (in_steps && (text == "-" || substr(text, 1, 2) == "- ") && (dash < 0 || n == dash)) {
		end_step()
		dash = n
		step++
		step_line = NR
		text = substr(text, 2)
		col = n + 1 + lead(text)
		text = substr(text, lead(text) + 1)
	} else if (n == body) {
		end_step()
		in_steps = (text ~ /^steps: *(#.*)?$/)
		in_defaults = (text ~ /^defaults: *(#.*)?$/)
		defaults_col = -1
		dash = -1
	} else if (in_defaults) {
		defaults(job, n, text)
	}

	if (match(text, /^[A-Za-z_][A-Za-z0-9_-]*:/) && (length(text) == RLENGTH || substr(text, RLENGTH + 1, 1) == " ")) {
		key = substr(text, 1, RLENGTH - 1)
		value = substr(text, RLENGTH + 1)
		sub(/^ +/, "", value)
		is_block = (value ~ /^[|>][-+0-9]* *(#.*)?$/)
		own = 0
		if (in_steps && step > 0) {
			if (step_col < 0)
				step_col = col
			own = (col == step_col)
			# The first line under with: sets its inputs' indent.
			if (in_with && col > step_col) {
				if (with_col < 0)
					with_col = col
				if (col == with_col) {
					input[key] = unquote(value)
					input_line[key] = NR
				}
			}
		}
		if (own) {
			in_with = (key == "with")
			with_col = -1
		}
		if (own && key == "id" && unquote(value) == "pins")
			step_pins = 1
		if (own && key == "uses")
			# GitHub resolves owner/repo whatever their case.
			step_uses = tolower(unquote(value))
		if (own && (key == "shell" || key == "working-directory") && env_key == "") {
			env_key = key
			env_line = NR
		}
		if (own && key == "run" && !is_block)
			step_run = unquote(value) "\n"
		if (is_block) {
			in_block = 1
			block_key = col
			content = -1
			capture = (own && key == "run")
		}
	}
	scan(text)
}
END {
	end_step()
}
AWK

# The literal scan, one pass over every workflow. PINS holds "<name>\t<value>"
# rows; a hit prints "<file>:<line>\t<name>\t<value>". A version token has no
# digit or dot on its left, and on its right neither a digit nor a dot that a
# digit follows. The leading "v" is optional on both sides, so v2.12.2 also
# catches `version: 2.12.2`. A `uses:` line is skipped: its version is the
# action's, which Dependabot bumps, not a tool's, and the version a tool's
# action installs is held to its pin by the tool records instead.
LITERALS=""
IFS= read -r -d '' LITERALS <<'AWK' || true
BEGIN {
	rows = split(ENVIRON["PINS"], row, "\n")
	for (i = 1; i <= rows; i++) {
		if (split(row[i], f, "\t") != 2)
			continue
		pins++
		name[pins] = f[1]
		value[pins] = f[2]
		token[pins] = f[2]
		sub(/^v/, "", token[pins])
	}
}
# A `uses:` line's @ref and the version comment Dependabot keeps after it
# are the action's own version, not a tool's, so only the part before the @
# is scanned. A docker:// image names the tool it runs, so it is scanned whole.
/^[ \t]*(-[ \t]+)?uses:/ {
	text = $0
	if (text !~ /docker:\/\//)
		sub(/@.*/, "", text)
	scan(text)
	next
}
{
	scan($0)
}
function scan(text,   i, s, at, p, before, after) {
	for (i = 1; i <= pins; i++) {
		s = text
		at = 0
		while ((p = index(s, token[i])) > 0) {
			at += p
			before = (at > 1) ? substr(text, at - 1, 1) : ""
			after = substr(text, at + length(token[i]), 2)
			if (before !~ /[0-9.]/ && after !~ /^[0-9]/ && after !~ /^\.[0-9]/) {
				print FILENAME ":" FNR "\t" name[i] "\t" value[i]
				break
			}
			s = substr(text, at + 1)
		}
	}
}
AWK

PROBLEMS=0
PINS_STEPS=0
PINS_READS=0
PINS_TOOLS=0

problem() {
	echo "FAIL: $*" >&2
	PROBLEMS=$((PROBLEMS + 1))
}

# check_literals <root> <workflow>...: no workflow names a pinned version.
check_literals() {
	local root="$1" line name pins="" where value
	shift
	while IFS= read -r line; do
		[[ "$line" =~ ^([A-Z][A-Z0-9_]*_VERSION)= ]] || continue
		name="${BASH_REMATCH[1]}"
		if [[ -z "${!name:-}" ]]; then
			problem "scripts/tool-versions.env: $name is empty"
			continue
		fi
		pins+="$name"$'\t'"${!name}"$'\n'
	done < "$root/scripts/tool-versions.env"
	while IFS=$'\t' read -r where name value; do
		problem "${where#"$root"/}: names $value, the $name pin; read it from scripts/tool-versions.env instead"
	done < <(PINS="$pins" awk "$LITERALS" "$@")
}

# check_reads <root> <workflow>: every steps.pins.outputs read in <workflow>
# resolves to a value its job's pins step writes before the read. Run from
# <root>, where each pins script expects to start.
check_reads() {
	local root="$1" wf="$2" rel work kind job step line field status
	local pins_step pins_line pin key value steps_before="$PINS_STEPS"
	local read_re='^\$\{\{ *steps\.pins\.outputs\.([A-Za-z_][A-Za-z0-9_-]*) *\}\}$'
	rel="${wf#"$root"/}"
	work="$WORK/${wf##*/}"
	awk -v prefix="$work." "$PARSER" "$wf" > "$work.records"

	# Run each pins step once, as CI would. <job>.step records where it is;
	# <job>.ran marks a script that exited 0, leaving its outputs in <job>.out.
	while IFS=$'\t' read -r kind job step line field; do
		case "$kind" in
			pins | pins-expr) ;;
			*) continue ;;
		esac
		PINS_STEPS=$((PINS_STEPS + 1))
		if [[ -e "$work.$job.step" ]]; then
			problem "$rel:$line: job '$job' has a second step with id: pins"
			continue
		fi
		printf '%s\t%s\n' "$step" "$line" > "$work.$job.step"
		if [[ ! -s "$field" ]]; then
			problem "$rel:$line: job '$job': its Read tool pins step has no run script"
		elif [[ "$kind" == pins-expr ]]; then
			problem "$rel:$line: job '$job': its Read tool pins script uses a \${{ }} expression, which only CI can evaluate; source scripts/tool-versions.env instead"
		else
			: > "$work.$job.out"
			status=0
			env -i PATH="$PATH" HOME="${HOME:-/}" GITHUB_OUTPUT="$work.$job.out" \
				bash --noprofile --norc -eo pipefail "$field" < /dev/null > "$work.$job.log" 2>&1 || status=$?
			if [[ "$status" -eq 0 ]]; then
				: > "$work.$job.ran"
			else
				problem "$rel:$line: job '$job': its Read tool pins script exits $status: $(tail -n 1 "$work.$job.log")"
			fi
		fi
	done < "$work.records"

	while IFS=$'\t' read -r kind job step line field; do
		case "$kind" in
			ref) ;;
			bad)
				problem "$rel:$line: job '$job' reads steps.pins.outputs without a .<name> after it"
				continue
				;;
			*) continue ;;
		esac
		PINS_READS=$((PINS_READS + 1))
		if [[ ! -e "$work.$job.step" ]]; then
			problem "$rel:$line: job '$job' reads steps.pins.outputs.$field but has no step with id: pins"
			continue
		fi
		IFS=$'\t' read -r pins_step pins_line < "$work.$job.step"
		if [[ "$step" -ne 0 && "$step" -le "$pins_step" ]]; then
			problem "$rel:$line: job '$job' reads steps.pins.outputs.$field in step $step, but its Read tool pins step (line $pins_line) is step $pins_step; move that step first"
		fi
		[[ -e "$work.$job.ran" ]] || continue
		pins_output "$work.$job.out" "$field"
		if [[ "$FOUND" -eq 0 ]]; then
			problem "$rel:$line: job '$job' reads steps.pins.outputs.$field, which its Read tool pins step (line $pins_line) does not write"
		elif [[ -z "$VALUE" ]]; then
			problem "$rel:$line: job '$job' reads steps.pins.outputs.$field, which its Read tool pins step (line $pins_line) writes empty"
		fi
	done < "$work.records"

	# Each setup-go and golangci-lint-action step installs the pinned version.
	# A read that does not resolve is already named above; one that resolves
	# must resolve to the pin.
	while IFS=$'\t' read -r kind job step line pin key value; do
		case "$kind" in
			tool) ;;
			env)
				problem "$rel:$line: job '$job': its Read tool pins step sets $key:, but its script must run with the default bash from the repository root, as this check runs it"
				continue
				;;
			defaults)
				if [[ "$job" == "*" && "$PINS_STEPS" -gt "$steps_before" ]] || [[ -e "$work.$job.step" ]]; then
					problem "$rel:$line: defaults.run applies to a Read tool pins step, whose script must run with the default bash from the repository root, as this check runs it"
				fi
				continue
				;;
			*) continue ;;
		esac
		PINS_TOOLS=$((PINS_TOOLS + 1))
		if [[ ! "$value" =~ $read_re ]]; then
			problem "$rel:$line: job '$job': its $key is ${value:-missing}; set it to \${{ steps.pins.outputs.<name> }}, written from \$$pin by a Read tool pins step"
			continue
		fi
		field="${BASH_REMATCH[1]}"
		[[ -e "$work.$job.ran" ]] || continue
		pins_output "$work.$job.out" "$field"
		if [[ "$FOUND" -eq 1 && -n "$VALUE" && "$VALUE" != "${!pin}" ]]; then
			problem "$rel:$line: job '$job': its $key reads steps.pins.outputs.$field, which is $VALUE, but scripts/tool-versions.env pins $pin=${!pin}"
		fi
	done < "$work.records"
}

# pins_output <out> <name>: FOUND is 1 when the $GITHUB_OUTPUT file <out>
# writes <name>, and VALUE is the last value it writes.
pins_output() {
	local out
	FOUND=0
	VALUE=""
	while IFS= read -r out; do
		if [[ "$out" == "$2="* ]]; then
			FOUND=1
			VALUE="${out#"$2="}"
		fi
	done < "$1"
}

# check_toolchain <root>: go.mod asks for the Go that CI installs.
check_toolchain() {
	local root="$1" line toolchain=""
	while read -r line; do
		if [[ "$line" =~ ^toolchain[[:space:]]+([^[:space:]]+) ]]; then
			toolchain="${BASH_REMATCH[1]}"
			break
		fi
	done < "$root/go.mod"
	if [[ "$toolchain" != "go$GO_VERSION" ]]; then
		problem "go.mod: toolchain is ${toolchain:-missing}; scripts/tool-versions.env pins GO_VERSION=$GO_VERSION, so it must be go$GO_VERSION"
	fi
}

main() {
	if [[ $# -gt 1 ]]; then
		echo "usage: $0 [root]" >&2
		return 64
	fi
	local root="${1:-}" workflows wf
	if [[ -z "$root" ]]; then
		root="$(dirname "${BASH_SOURCE[0]}")/../.."
	fi
	root="$(cd "$root" && pwd)"
	cd "$root"
	# shellcheck source=scripts/tool-versions.env
	. "$root/scripts/tool-versions.env"

	WORK="$(mktemp -d)"
	trap 'rm -rf "$WORK"' EXIT

	shopt -s nullglob
	workflows=("$root"/.github/workflows/*.yml "$root"/.github/workflows/*.yaml)
	shopt -u nullglob
	if [[ ${#workflows[@]} -eq 0 ]]; then
		problem "no workflows under $root/.github/workflows"
	else
		check_literals "$root" "${workflows[@]}"
		for wf in "${workflows[@]}"; do
			check_reads "$root" "$wf"
		done
	fi
	check_toolchain "$root"

	if [[ "$PROBLEMS" -gt 0 ]]; then
		echo "workflow pins: $PROBLEMS problem(s); every workflow version comes from scripts/tool-versions.env" >&2
		return 1
	fi
	echo "workflow pins: $PINS_READS reads of $PINS_STEPS Read tool pins steps resolve; $PINS_TOOLS Go and golangci-lint installs use their pins; no workflow names a pinned version; go.mod uses go$GO_VERSION"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	set -euo pipefail
	main "$@"
fi
