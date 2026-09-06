import {
  addDrops,
  type DropCounts,
  type Level,
  LEVELS,
  MAX_FRAME,
} from "./protocol.ts";

interface Entry {
  frame: Uint8Array;
  level: Level;
  order: number;
}

class Ring {
  #items: Array<Entry | undefined>;
  #head = 0;
  length = 0;
  constructor(capacity: number) {
    this.#items = new Array(capacity);
  }
  peek(): Entry | undefined {
    return this.#items[this.#head];
  }
  push(entry: Entry): void {
    this.#items[(this.#head + this.length++) % this.#items.length] = entry;
  }
  shift(): Entry | undefined {
    if (this.length === 0) return undefined;
    const entry = this.#items[this.#head];
    this.#items[this.#head] = undefined;
    this.#head = (this.#head + 1) % this.#items.length;
    this.length--;
    return entry;
  }
}

// Credit includes frames held by the one socket write in flight. Reserved high
// priority space cannot be consumed by low severity traffic.
export class LogQueue {
  readonly maximum: number;
  readonly reserve: number;
  readonly slots: number;
  readonly dropped: DropCounts = [0, 0, 0, 0];
  #low: Ring;
  #high: Ring;
  #order = 0;
  bytes = 0;
  records = 0;

  constructor(maximum = 128 * 1024, reserve = 32 * 1024, slots = 512) {
    if (
      maximum < MAX_FRAME + 132 || reserve < 0 || reserve >= maximum ||
      slots < 4
    ) {
      throw new Error("invalid log queue bounds");
    }
    this.maximum = maximum;
    this.reserve = reserve;
    this.slots = slots;
    this.#low = new Ring(slots);
    this.#high = new Ring(slots);
  }

  get waiting(): number {
    return this.#low.length + this.#high.length;
  }

  drop(level: Level, count = 1): void {
    const counts: DropCounts = [0, 0, 0, 0];
    counts[LEVELS.indexOf(level)] = count;
    addDrops(this.dropped, counts);
  }

  add(frame: Uint8Array, level: Level): boolean {
    if (
      frame.length < 5 || frame.length > MAX_FRAME + 4 ||
      !LEVELS.includes(level)
    ) return false;
    const high = LEVELS.indexOf(level) >= 2;
    const limit = this.maximum - (high ? 0 : this.reserve);
    const slots = this.slots -
      (high ? 0 : Math.max(1, Math.floor(this.slots / 4)));
    const weight = frame.length + 128;
    if (high) {
      while (
        (this.bytes + weight > limit || this.records >= slots) &&
        this.#low.length > 0
      ) {
        const old = this.#low.shift()!;
        this.bytes -= old.frame.length + 128;
        this.records--;
        this.drop(old.level);
      }
    }
    if (this.bytes + weight > limit || this.records >= slots) {
      this.drop(level);
      return false;
    }
    (high ? this.#high : this.#low).push({
      frame,
      level,
      order: ++this.#order,
    });
    this.bytes += weight;
    this.records++;
    return true;
  }

  take(maximum: number): { entries: Entry[]; release(): void } {
    const entries: Entry[] = [];
    let length = 0;
    let weight = 0;
    while (this.waiting > 0) {
      const low = this.#low.peek(), high = this.#high.peek();
      const ring =
        low === undefined || (high !== undefined && high.order < low.order)
          ? this.#high
          : this.#low;
      const entry = ring.peek()!;
      if (length + entry.frame.length > maximum) break;
      ring.shift();
      entries.push(entry);
      length += entry.frame.length;
      weight += entry.frame.length + 128;
    }
    let released = false;
    return {
      entries,
      release: () => {
        if (released) return;
        released = true;
        this.bytes -= weight;
        this.records -= entries.length;
        entries.length = 0;
      },
    };
  }
}
