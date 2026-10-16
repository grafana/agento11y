import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as sleep } from "node:timers/promises";
import {
  command,
  loopback,
  matchesVersion,
  options,
  sandboxPolicy,
} from "./test-hosts.mjs";

for (const [reported, version, expected] of [
  ["2.0.22\n", "2.0.22", true],
  ["opencode v2.0.22\n", "2.0.22", true],
  ["opencode v2.0.29\n", "2.0.29", true],
  ["1.18.29\n", "1.18.29", true],
  ["opencode v2.0.29", "2.0.22", false],
  ["2.0.220", "2.0.22", false],
]) {
  test(`matches reported version ${JSON.stringify(reported)} against ${version}`, () => {
    assert.equal(matchesVersion(reported, version), expected);
  });
}

for (const [args, versions] of [
  [[], ["1.2.16", "1.3.13", "1.18.29", "2.0.22", "2.0.29"]],
  [
    ["--scenario", "compaction"],
    ["2.0.22", "2.0.29"],
  ],
  [["--scenario", "compaction", "--version", "2.0.29"], ["2.0.29"]],
]) {
  test(`selects hosts for ${JSON.stringify(args)}`, () => {
    assert.deepEqual(options(args).versions, versions);
  });
}
for (const [args, error] of [
  [["--scenario", "invalid"], /--scenario must be basic or compaction/],
  [["--scenario"], /--scenario must be basic or compaction/],
  [["--scenario", "compaction", "--version", "1.18.29"], /v2 hosts only/],
  [["--scenario", "compaction", "--imports"], /requires real v2 hosts/],
]) {
  test(`rejects incompatible scenario ${JSON.stringify(args)}`, () => {
    assert.throws(() => options(args), error);
  });
}

for (const stream of [false, true]) {
  test(`compaction provider sends overflow, summary usage and final reply (stream=${stream})`, async () => {
    const receiver = await loopback("compaction");
    try {
      const request = async (text) => {
        const response = await fetch(
          `${receiver.url}/model/v1/chat/completions`,
          {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({
              model: "packed-harness",
              stream,
              messages: [{ role: "user", content: text }],
            }),
          },
        );
        return { status: response.status, text: await response.text() };
      };
      const overflow = await request("packed-package-compatibility-turn");
      assert.equal(overflow.status, 400);
      assert.equal(
        JSON.parse(overflow.text).error.code,
        "context_length_exceeded",
      );
      const summary = await request("## Objective");
      assert.equal(summary.status, 200);
      assert(summary.text.includes("## Objective"));
      assert(!summary.text.includes("PACKED_HOST_OK"));
      assert(summary.text.includes('"prompt_tokens":100'));
      assert(summary.text.includes('"completion_tokens":20'));
      const final = await request(
        "## Objective\n- Complete packed-package-compatibility-turn using the requested reply; no tools.",
      );
      assert.equal(final.status, 200);
      assert(final.text.includes("PACKED_HOST_OK"));
      assert(final.text.includes('"prompt_tokens":7'));
      assert(final.text.includes('"completion_tokens":3'));
      assert.equal(receiver.state.models.length, 3);
      assert.deepEqual(receiver.state.errors, []);
    } finally {
      await receiver.close();
    }
  });
}

for (const value of ["999", "1000.5", "NaN", "Infinity", "-1", undefined]) {
  test(`rejects timeout ${value}`, () => {
    assert.throws(
      () => options(["--timeout-ms", value]),
      /--timeout-ms must be an integer of at least 1000 milliseconds/,
    );
  });
}

test("accepts the minimum timeout", () => {
  assert.equal(options(["--imports", "--timeout-ms", "1000"]).timeout, 1000);
});

for (const flag of ["--version", "--versions"]) {
  test(`names both host options when ${flag} conflicts with imports`, () => {
    assert.throws(
      () => options(["--imports", flag, "2.0.22"]),
      /--imports does not run hosts\. Omit --version and --versions\./,
    );
  });
}

for (const [name, source, expected] of [
  ["success", "console.log('fixture output')", "fixture output\n"],
  [
    "failure",
    "console.error('fixture failure'); process.exit(42)",
    /failed \(42\).*fixture failure/s,
  ],
  ["timeout", "setInterval(() => {}, 1000)", /timed out after 50ms/],
]) {
  test(`command ${name} cleans signal listeners`, async () => {
    const counts = [
      process.listenerCount("SIGINT"),
      process.listenerCount("SIGTERM"),
    ];
    const result = command(process.execPath, ["-e", source], {
      timeout: name === "timeout" ? 50 : 5000,
    });
    if (typeof expected === "string") assert.equal(await result, expected);
    else await assert.rejects(result, expected);
    assert.deepEqual(
      [process.listenerCount("SIGINT"), process.listenerCount("SIGTERM")],
      counts,
    );
  });
}

test("command spawn error cleans signal listeners", async () => {
  const counts = [
    process.listenerCount("SIGINT"),
    process.listenerCount("SIGTERM"),
  ];
  await assert.rejects(
    command("/nonexistent-packed-harness-command", []),
    /unavailable/,
  );
  assert.deepEqual(
    [process.listenerCount("SIGINT"), process.listenerCount("SIGTERM")],
    counts,
  );
});

for (const path of [
  "/Library/Application Support/opencode",
  "/Library/Managed Preferences",
  "/System/Volumes/Data/Library/Application Support/opencode",
  "/System/Volumes/Data/Library/Managed Preferences",
]) {
  test(`sandbox policy denies machine configuration subtree ${path}`, () => {
    assert.ok(
      sandboxPolicy('/tmp/fixture-"root').includes(
        `(deny file-read* (subpath ${JSON.stringify(path)}))`,
      ),
    );
  });
}

test("sandbox retains loopback-only networking and temporary writes", () => {
  const root = '/tmp/fixture-"root';
  const policy = sandboxPolicy(root);
  assert.ok(policy.includes("(deny network*)"));
  assert.ok(
    policy.includes('(allow network-inbound (local ip "localhost:*"))'),
  );
  assert.ok(
    policy.includes('(allow network-outbound (remote ip "localhost:*"))'),
  );
  assert.ok(policy.includes("(deny file-write*)"));
  assert.ok(
    policy.includes(`(allow file-write* (subpath ${JSON.stringify(root)})`),
  );
});

async function waitFor(check) {
  for (let i = 0; i < 100; i++) {
    if (await check()) return;
    await sleep(50);
  }
  assert.fail("Timed out waiting for fixture process state");
}

for (const signal of ["SIGINT", "SIGTERM"]) {
  for (const ignore of [false, true]) {
    test(`${signal} stops detached children (ignore=${ignore})`, {
      timeout: 15000,
    }, async () => {
      const root = await mkdtemp(join(tmpdir(), "packed-command-signal-"));
      const pidsFile = join(root, "pids.json");
      const heartbeat = join(root, "heartbeat");
      const wrapperFile = join(root, "wrapper.mjs");
      const childFile = join(root, "child.mjs");
      const leafFile = join(root, "leaf.mjs");
      const handlers = ignore
        ? "process.on('SIGINT', () => {}); process.on('SIGTERM', () => {});"
        : "";
      await writeFile(
        leafFile,
        `import { writeFileSync } from 'node:fs';
${handlers}
setInterval(() => writeFileSync(${JSON.stringify(heartbeat)}, String(Date.now())), 25);
process.send('ready');
`,
      );
      await writeFile(
        childFile,
        `import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';
${handlers}
const leaf = spawn(process.execPath, [${JSON.stringify(leafFile)}], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
leaf.once('message', () => writeFileSync(${JSON.stringify(pidsFile)}, JSON.stringify([process.pid, leaf.pid])));
setInterval(() => {}, 1000);
`,
      );
      await writeFile(
        wrapperFile,
        `import { command } from ${JSON.stringify(new URL("./test-hosts.mjs", import.meta.url).href)};
try {
  await command(process.execPath, [${JSON.stringify(childFile)}], { timeout: 30000 });
} catch (error) {
  console.error(error.message);
  process.exitCode = error.signal === 'SIGINT' ? 130 : error.signal === 'SIGTERM' ? 143 : 1;
}
`,
      );
      const wrapper = spawn(process.execPath, [wrapperFile], {
        stdio: ["ignore", "pipe", "pipe"],
      });
      const closed = once(wrapper, "close");
      let output = "";
      wrapper.stderr.on("data", (chunk) => {
        output += chunk;
      });
      let pids;
      try {
        await waitFor(async () => {
          try {
            pids = JSON.parse(await readFile(pidsFile, "utf8"));
            await readFile(heartbeat, "utf8");
            return true;
          } catch (error) {
            if (error.code === "ENOENT") return false;
            throw error;
          }
        });
        wrapper.kill(signal);
        const [code, exitSignal] = await closed;
        assert.equal(exitSignal, null);
        assert.equal(code, signal === "SIGINT" ? 130 : 143);
        assert.match(output, new RegExp(`cancelled \\(${signal}\\)`));
        await waitFor(() => {
          try {
            process.kill(pids[0], 0);
            return false;
          } catch (error) {
            if (error.code === "ESRCH") return true;
            throw error;
          }
        });
        const stopped = await readFile(heartbeat, "utf8");
        await sleep(150);
        assert.equal(
          await readFile(heartbeat, "utf8"),
          stopped,
          "grandchild kept running",
        );
      } finally {
        if (pids) {
          try {
            process.kill(-pids[0], "SIGKILL");
          } catch (error) {
            assert.equal(error.code, "ESRCH");
          }
        }
        wrapper.kill("SIGKILL");
        await closed;
        await rm(root, { recursive: true, force: true });
      }
    });
  }
}
