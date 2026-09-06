import { assert, assertThrows } from "@std/assert";
import { isId, newId } from "./mod.ts";

Deno.test("operational identities have one typed canonical encoding", () => {
  const seen = new Set<string>();
  for (const prefix of ["nod", "sbx", "wrk", "ctx", "job", "srv", "uis"]) {
    for (let index = 0; index < 128; index++) {
      const value = newId(prefix);
      assert(isId(value, prefix));
      assert(!isId(value, "bad"));
      assert(!seen.has(value));
      seen.add(value);
    }
  }
  for (const prefix of ["", "a", "abcd", "SBX", "s1x", "s-x", "éx"]) {
    assertThrows(() => newId(prefix), TypeError);
  }
  for (
    const value of [
      undefined,
      null,
      1,
      "",
      "sbx-abcdefgh",
      "sbx-ABCDEFGHIJ",
      "sbx-abcdefghi_",
      "sbx-abcdefghijk",
      "sbx-abcdefghié",
      "sbx-abcdefghij/",
      "wrk-abcdefghij",
    ]
  ) assert(!isId(value, "sbx"));
});
