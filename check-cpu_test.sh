#!/usr/bin/env bash
set -euo pipefail
SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
CHECK="$SOURCE_ROOT/docker/rootfs/usr/local/bin/the8020-check-cpu"
FIXTURE_ROOT=$(mktemp -d)
trap 'rm -rf -- "$FIXTURE_ROOT"' EXIT

base='fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 syscall nx lm nopl cpuid pni cx16 x2apic hypervisor lahf_lm cpuid_fault'
v2="$base ssse3 sse4_1 sse4_2 popcnt"
v3="$v2 fma movbe xsave avx f16c abm bmi1 avx2 bmi2"
for level in base v2 v3; do
  printf 'processor\t: 0\nmodel name\t: test\nflags\t\t: %s\n\nprocessor\t: 1\nflags\t\t: %s\n' \
    "${!level}" "${!level}" > "$FIXTURE_ROOT/$level"
done

check() {
  local result=0
  THE8020_CPU_ARCHITECTURE=${ARCH:-x86_64} THE8020_CPUINFO="$FIXTURE_ROOT/$1" \
    "$BASH" "$CHECK" > "$FIXTURE_ROOT/output" 2>&1 || result=$?
  echo "$result"
}

# QEMU kvm64 ("Common KVM processor") lacks x86-64-v2 and must be refused.
[[ $(check base) == 1 ]]
grep -Fq 'does not provide x86-64-v2 (missing: popcnt sse4_1 sse4_2 ssse3)' "$FIXTURE_ROOT/output"
grep -Fq 'set the VM CPU type to host or x86-64-v3' "$FIXTURE_ROOT/output"

# x86-64-v2 runs the platform; missing AVX2 is reported without failing.
[[ $(check v2) == 0 ]]
grep -Fq 'x86-64-v3 is unavailable (missing: avx avx2 bmi1 bmi2 f16c fma abm movbe xsave)' "$FIXTURE_ROOT/output"

[[ $(check v3) == 0 ]]
[[ ! -s "$FIXTURE_ROOT/output" ]]

# Other architectures are left to the existing uname -m switches.
[[ $(ARCH=aarch64 check base) == 0 ]]
[[ ! -s "$FIXTURE_ROOT/output" ]]

[[ $(THE8020_CPU_CHECK=skip check base) == 0 ]]
grep -Fq 'check skipped by THE8020_CPU_CHECK=skip (missing x86-64-v2: popcnt sse4_1 sse4_2 ssse3)' "$FIXTURE_ROOT/output"

[[ $(check missing) == 1 ]]
grep -Fq 'cannot read' "$FIXTURE_ROOT/output"

result=0
"$BASH" "$CHECK" --unexpected > /dev/null 2>&1 || result=$?
[[ "$result" == 2 ]]

echo 'CPU level checks passed'
