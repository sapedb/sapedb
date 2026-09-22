/**
 * The order → payment → ledger example, running.
 *
 * This is the flow every database gets asked for and normally answers with an
 * interactive transaction: begin, read the order, check it is still unpaid,
 * insert the payment, post two ledger lines, commit — with the row locked and
 * everybody else waiting on whatever the client does in between.
 *
 * Here there is no begin. `orders.pay` is one declared operation made of four
 * steps, and the operation is the transaction. Its cost is known before it
 * runs, the client cannot hold it open, and the conditions it checks are
 * written down in schema.json where they can be read by someone deciding
 * whether to trust it.
 *
 * Run it:
 *
 *   SAPEDB_SERVER_BIN=…/sapedbd SAPEDB_CLI_BIN=…/sapedb node examples/ledger/run.mjs
 *
 * SAPEDB_CLIENT may point at the built driver; it defaults to the sibling
 * ecosy-sapedb package in this workspace.
 *
 * Each of the three reads in schema.json declares a `projection`, and each one
 * leaves something out on purpose — that is the part worth copying. `orders.get`
 * does not return `customer`: a payment flow asking whether an order is paid has
 * no use for who placed it, and a field that is not named here never leaves the
 * database. `payments.of_order` and `entries.of_account` do not return `order`
 * and `account`: both scans are bounded to the one value the caller passed in,
 * so returning it would be sending the argument back, fifty or five hundred
 * times. A read with no projection is valid and the store will serve it — it
 * just hands the caller a document of unknown shape, which is what
 * `sapedb-types` then has to write down as `Record<string, unknown>`.
 */

import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createServer } from "node:net";

const here = dirname(fileURLToPath(import.meta.url));
const SERVER = process.env.SAPEDB_SERVER_BIN;
const CLI = process.env.SAPEDB_CLI_BIN;
const CLIENT = process.env.SAPEDB_CLIENT ?? resolve(here, "../../../ecosy-sapedb/dist");

if (!SERVER || !CLI) {
  console.error("set SAPEDB_SERVER_BIN and SAPEDB_CLI_BIN to the built binaries");
  process.exit(2);
}

const { Client } = await import(pathToFileURL(join(CLIENT, "client/index.mjs")));
const { nodeTransport } = await import(pathToFileURL(join(CLIENT, "node/index.mjs")));
const { sign } = await import(pathToFileURL(join(CLIENT, "signer/index.mjs")));

const SECRET = "an-example-secret";
const PASSWORD = "an-example-password";

const say = (line) => console.log(line);
const money = (n) => (n < 0 ? `-${(-n).toFixed(2)}` : ` ${n.toFixed(2)}`);

async function freePort() {
  return new Promise((ok, no) => {
    const probe = createServer();
    probe.once("error", no);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => ok(port));
    });
  });
}

const dir = mkdtempSync(join(tmpdir(), "sapedb-ledger-"));
const env = { ...process.env, SAPEDB_SECRET: SECRET, SAPEDB_DIR: dir, SAPEDB_ACCOUNT: "acme", SAPEDB_DB: "books" };

say(`a database in ${dir}`);
execFileSync(CLI, ["apply", join(here, "schema.json")], { env, stdio: "inherit" });
say("declared: three collections, five operations, one of them a batch of four steps\n");

const port = await freePort();
const server = spawn(SERVER, [], {
  env: { ...env, SAPEDB_INSECURE: "1", SAPEDB_ADDR: `127.0.0.1:${port}` },
  stdio: ["ignore", "pipe", "inherit"],
});
await new Promise((ok, no) => {
  const timer = setTimeout(() => no(new Error("the server did not announce itself")), 5000);
  server.stdout.on("data", (chunk) => String(chunk).includes("listening") && (clearTimeout(timer), ok()));
  server.once("error", no);
});

const sig = await sign({ accountId: "acme", password: PASSWORD, dbname: "books" }, { secret: SECRET });
const url = `sapedb://acme:${PASSWORD}@127.0.0.1:${port}/books?sig=${sig}`;
const client = new (Client({ transport: nodeTransport({ insecure: true }), mode: "bound", requestTimeout: 5000 }))();

let failed = false;
const must = (what, ok) => {
  say(`${ok ? "  ok  " : "  NO  "} ${what}`);
  if (!ok) failed = true;
};

try {
  const at = Date.now();

  say("an order is placed");
  await client.invoke(url, "orders.place", { id: "ord-1001", customer: "cus-7", total: 249.5 }, { write: true });
  const placed = await client.invoke(url, "orders.get", { id: "ord-1001" });
  say(`  ord-1001  ${placed.rows[0].status}  ${money(placed.rows[0].total)}\n`);

  say("it is paid — one call, four writes, one transaction");
  const paid = await client.invoke(
    url,
    "orders.pay",
    { order: "ord-1001", amount: 249.5, at, reference: "stripe_pi_3Qx" },
    { write: true },
  );
  say(`  ${paid.changed} documents changed\n`);

  const after = await client.invoke(url, "orders.get", { id: "ord-1001" });
  const payments = await client.invoke(url, "payments.of_order", { order: "ord-1001" });
  const cash = await client.invoke(url, "entries.of_account", { account: "assets:cash" });
  const sales = await client.invoke(url, "entries.of_account", { account: "income:sales" });

  say("what the books now say");
  say(`  orders      ord-1001            ${after.rows[0].status}`);
  say(`  payments    ${payments.rows[0].reference}      ${money(payments.rows[0].amount)}  → ${payments.rows[0].id}`);
  say(`  entries     assets:cash  debit  ${money(cash.rows[0].amount)}`);
  say(`  entries     income:sales credit ${money(sales.rows[0].amount)}`);
  const balance = cash.rows.reduce((t, e) => t + e.amount, 0) - sales.rows.reduce((t, e) => t + e.amount, 0);
  say(`  balance                        ${money(balance)}\n`);

  must("the order is paid", after.rows[0].status === "paid");
  /* The payment's own key, generated by the store inside the transaction, is
     what the two ledger lines were written against. That is the step wiring
     — `{ "step": "payment", "field": "key" }` — proved end to end, and it is
     a stronger reading than the old one: `payments.of_order` no longer hands
     back `order`, because its projection leaves out the argument the scan was
     bounded by, so the link has to be read from the side that was not known
     before the call. */
  must("the ledger lines point at the payment that made them", cash.rows[0].payment === payments.rows[0].id);
  must("the projection is what came back, and not the whole document", !("order" in payments.rows[0]));
  must("one debit and one credit", cash.rows.length === 1 && sales.rows.length === 1);
  must("the ledger balances", balance === 0);

  say("\nthe same payment arrives again — a retry, a double-click, a webhook replayed");
  let refused;
  try {
    await client.invoke(
      url,
      "orders.pay",
      { order: "ord-1001", amount: 249.5, at, reference: "stripe_pi_3Qx" },
      { write: true },
    );
  } catch (error) {
    refused = error;
    say(`  refused: ${error.code} — ${error.message}`);
  }
  // The code, not the sentence. A driver deciding whether to retry cannot read
  // prose, and "condition" is the one answer that means: the world moved, read
  // it again — retrying this exact call will fail the same way forever.
  must("the second payment was refused", refused?.code === "condition");

  const stillOne = await client.invoke(url, "payments.of_order", { order: "ord-1001" });
  const stillCash = await client.invoke(url, "entries.of_account", { account: "assets:cash" });
  must("no second payment was written", stillOne.rows.length === 1);
  must("no ledger line was left behind by the refused call", stillCash.rows.length === 1);

  say("\nand an order paid the wrong amount");
  await client.invoke(url, "orders.place", { id: "ord-1002", customer: "cus-7", total: 40 }, { write: true });
  let wrong;
  try {
    await client.invoke(url, "orders.pay", { order: "ord-1002", amount: 39.99, at, reference: "short" }, { write: true });
  } catch (error) {
    wrong = error;
    say(`  refused: ${error.code} — ${error.message}`);
  }
  must("a payment that does not match the total was refused", wrong?.code === "condition");
  const short = await client.invoke(url, "orders.get", { id: "ord-1002" });
  must("the order it half-wrote is untouched", short.rows[0].status === "awaiting_payment");
  must("the ledger still balances", (await client.invoke(url, "entries.of_account", { account: "assets:cash" })).rows.length === 1);
} finally {
  await client.close();
  server.kill("SIGTERM");
}

say(failed ? "\nsomething is wrong" : "\nall of it held");
process.exit(failed ? 1 : 0);
