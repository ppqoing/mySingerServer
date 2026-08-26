import { byteText } from "./format";

test("byteText formats B/KB/MB/GB/TB tiers", () => {
  expect(byteText(0)).toBe("0 B");
  expect(byteText(512)).toBe("512 B");
  expect(byteText(1023)).toBe("1023 B");
  expect(byteText(1024)).toBe("1.0 KB");
  expect(byteText(1536)).toBe("1.5 KB");
  expect(byteText(1024 ** 2)).toBe("1.0 MB");
  expect(byteText(1024 ** 3)).toBe("1.0 GB");
  expect(byteText(4_294_967_296)).toBe("4.0 GB");
  expect(byteText(1024 ** 4)).toBe("1.0 TB");
  expect(byteText(2.5 * 1024 ** 4)).toBe("2.5 TB");
});
