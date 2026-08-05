import { describe, expect, test } from "vitest";
import {
  getOverlayStackDiagnosticsForTests,
  overlayLayer,
  registerOverlay
} from "./overlayStack";

describe("overlayStack", () => {
  test("does not retain detached nested modal snapshots while a drawer remains open", async () => {
    const appRoot = document.createElement("main");
    const drawerScrim = document.createElement("button");
    const drawer = document.createElement("aside");
    document.body.append(appRoot, drawerScrim, drawer);
    const drawerHandle = registerOverlay([drawerScrim, drawer], {
      layer: overlayLayer.drawer,
      restoreFocus: null
    });

    try {
      const baseline = getOverlayStackDiagnosticsForTests().snapshots;

      for (let index = 0; index < 4; index += 1) {
        const backdrop = document.createElement("div");
        const dialog = document.createElement("div");
        backdrop.append(dialog);
        document.body.append(backdrop);
        const modalHandle = registerOverlay([backdrop, dialog], {
          layer: overlayLayer.modal,
          restoreFocus: drawer
        });

        modalHandle.release();
        backdrop.remove();
        await Promise.resolve();

        expect(getOverlayStackDiagnosticsForTests().snapshots).toBe(baseline);
      }
    } finally {
      drawerHandle.release();
      appRoot.remove();
      drawerScrim.remove();
      drawer.remove();
    }
  });
});
