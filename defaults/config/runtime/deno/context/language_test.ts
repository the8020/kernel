import { assertEquals, assertThrows } from "@std/assert";
import { context, type ExecutionContext, withLanguage } from "./mod.ts";
import { installContextProvider } from "./runtime.ts";

Deno.test("language follows async scopes without leaking between requests", async () => {
  assertEquals(context.language, undefined);
  await Promise.all(
    ["sk", "pt"].map((language) =>
      withLanguage(language, async () => {
        await Promise.resolve();
        assertEquals(context.language, language);
        withLanguage("en", () => assertEquals(context.language, "en"));
        assertEquals(context.language, language);
      })
    ),
  );
  assertEquals(context.language, undefined);
  assertThrows(() => withLanguage("", () => {}), TypeError);
});

Deno.test("language augments the frozen context without replacing execution identity", () => {
  const original = Object.freeze({
    username: "alice",
    type: "program",
    id: "example/app/show",
  }) as ExecutionContext;
  const restore = installContextProvider(() => original);
  try {
    assertEquals(context.current, original);
    withLanguage("sk", () => {
      assertEquals(context.current.language, "sk");
      assertEquals(context.username, "alice");
      assertEquals(context.id, "example/app/show");
      assertEquals(Object.isFrozen(context.current), true);
    });
    assertEquals(context.current, original);
    assertEquals(original.language, undefined);
  } finally {
    restore();
  }
});
