import indexHtml from "../../index.html?raw";
import packageJson from "../../package.json";
import viteConfig from "../../vite.config";

const startupScript = indexHtml.match(/<script>([\s\S]*?)<\/script>/)?.[1] ?? "";

function runIndexStartup(initialHash: string) {
  const location = { hash: initialHash };
  new Function("window", startupScript)({ location });
  return location.hash;
}

test("initializes an empty hash at the overview workspace", () => {
  expect(runIndexStartup("")).toBe("#/overview");
});

test("preserves a nonempty hash during startup", () => {
  expect(runIndexStartup("#/groups")).toBe("#/groups");
});

test("keeps Vite multi-page entries relative to the package root", () => {
  const config = viteConfig as {
    build?: {
      rollupOptions?: {
        input?: unknown;
      };
    };
  };

  expect(config.build?.rollupOptions?.input).toEqual({
    index: "index.html",
    groups: "groups.html",
  });
});

test("loads the Vite config without writing a bundled temp config", () => {
  expect(packageJson.scripts.build).toContain("--configLoader runner");
});
