let retained: number[] = [];

export default function (count = 0, cpuMilliseconds = 0): number {
  retained = Array.from({ length: count }, (_, index) => index);
  const end = performance.now() + cpuMilliseconds;
  while (performance.now() < end) Math.sqrt(performance.now());
  return retained.length;
}
