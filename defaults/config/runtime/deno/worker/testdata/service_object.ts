export default {
  fetch(request: Request) {
    return Response.json({ path: new URL(request.url).pathname });
  },
  get openapi(): never {
    throw new Error("documentation must never be read during service startup");
  },
};
