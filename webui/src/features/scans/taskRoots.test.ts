import { describe, expect, test } from "vitest";
import { addTaskRoot } from "./taskRoots";

describe("addTaskRoot", () => {
  test("rejects duplicate roots after slash and case normalization", () => {
    expect(addTaskRoot(["D:\\Media"], "d:/media").kind).toBe("duplicate");
  });

  test("rejects a candidate already covered by an existing root", () => {
    expect(addTaskRoot(["D:\\Media"], "D:\\Media\\Photos").kind).toBe("covered");
  });

  test("replaces child roots when their parent is added", () => {
    expect(addTaskRoot(["D:\\Media\\Photos"], "D:\\Media")).toEqual({
      kind: "replace", roots: ["D:\\Media"], covered: ["D:\\Media\\Photos"]
    });
  });

  test("rejects relative paths without consulting the filesystem", () => {
    expect(addTaskRoot([], "Media\\relative").kind).toBe("invalid");
  });
});
