const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const test = require("node:test");

const { parseExpectedChecksum, verifyChecksum } = require("./install");

test("parseExpectedChecksum matches the exact release filename", () => {
  const filename = "shan_1.2.3_darwin_arm64.tar.gz";
  const checksum = "a".repeat(64);
  const data = [
    `${"b".repeat(64)}  ${filename}.sig`,
    `${checksum}  ${filename}`,
  ].join("\n");

  assert.equal(parseExpectedChecksum(data, filename), checksum);
});

test("parseExpectedChecksum accepts the standard binary marker", () => {
  const filename = "shan_1.2.3_linux_amd64.tar.gz";
  const checksum = "C".repeat(64);

  assert.equal(
    parseExpectedChecksum(`${checksum} *${filename}\n`, filename),
    checksum.toLowerCase()
  );
});

test("parseExpectedChecksum rejects missing and malformed entries", () => {
  const filename = "shan_1.2.3_darwin_arm64.tar.gz";

  assert.throws(
    () => parseExpectedChecksum(`${"a".repeat(64)}  ${filename}.sig\n`, filename),
    /No valid checksum/
  );
  assert.throws(
    () => parseExpectedChecksum(`not-a-sha256  ${filename}\n`, filename),
    /No valid checksum/
  );
});

test("verifyChecksum accepts a match and rejects a mismatch", () => {
  const filename = "shan_1.2.3_darwin_arm64.tar.gz";
  const tarball = Buffer.from("synthetic release bytes");
  const checksum = crypto.createHash("sha256").update(tarball).digest("hex");

  assert.equal(
    verifyChecksum(tarball, `${checksum}  ${filename}\n`, filename),
    checksum
  );
  assert.throws(
    () => verifyChecksum(tarball, `${"0".repeat(64)}  ${filename}\n`, filename),
    /Checksum mismatch/
  );
});
