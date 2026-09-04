/** Coalesces absolute supervisor snapshots while keeping at most one send active. */
export class SnapshotPublisher {
  #enabled = false;
  #dirty = false;
  #scheduled = false;
  #inFlight?: Promise<void>;

  constructor(
    private readonly snapshot: () => unknown,
    private readonly send: (snapshot: unknown) => Promise<void>,
    private readonly report: (error: unknown) => void = () => {},
  ) {}

  enable(): void {
    this.#enabled = true;
    if (this.#dirty) this.#schedule();
  }

  markDirty(): void {
    this.#dirty = true;
    if (this.#enabled) this.#schedule();
  }

  flush(): Promise<void> {
    if (!this.#enabled || !this.#dirty) {
      return this.#inFlight ?? Promise.resolve();
    }
    if (this.#inFlight !== undefined) return this.#inFlight;
    this.#inFlight = this.#publish().finally(() => {
      this.#inFlight = undefined;
      if (this.#dirty) this.#schedule();
    });
    return this.#inFlight;
  }

  async #publish(): Promise<void> {
    while (this.#dirty) {
      this.#dirty = false;
      try {
        await this.send(this.snapshot());
      } catch (error) {
        this.report(error);
        return;
      }
    }
  }

  #schedule(): void {
    if (this.#scheduled) return;
    this.#scheduled = true;
    queueMicrotask(() => {
      this.#scheduled = false;
      void this.flush();
    });
  }
}
