// The library the lab tests run in Thunderbird, in front of every script
// they send over Marionette (thunderbird_test.go). It runs with the
// privileges of Thunderbird's own code and uses its mail interfaces: what
// the folder pane, the message list and the compose window call when a
// person clicks. Written against Thunderbird 153 ESR.
//
// A folder is named by its path on the server ("INBOX", "Projects/Küche").
// A message is named by its Message-ID, without the angle brackets.

const { MailServices } = ChromeUtils.importESModule("resource:///modules/MailServices.sys.mjs");
const { MailUtils } = ChromeUtils.importESModule("resource:///modules/MailUtils.sys.mjs");

const win = Services.wm.getMostRecentWindow("mail:3pane");
const msgWindow = win.msgWindow;
const account = MailServices.accounts.accounts.find(a => a.incomingServer.type == "imap");
const server = account.incomingServer;
const root = server.rootFolder;
const host = Services.io.newURI(server.serverURI).host;
const Flags = Ci.nsMsgFolderFlags;
const MsgFlags = Ci.nsMsgMessageFlags;

// An nsresult with the highest bit set is a failure.
const succeeded = status => !(status & 0x80000000);
const sleep = ms => new Promise(r => win.setTimeout(r, ms));

// until waits for test() to return something, asking every 100 ms.
async function until(what, ms, test) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await test();
    if (v) {
      return v;
    }
    if (Date.now() > end) {
      throw new Error("timed out after " + ms + " ms waiting for " + what);
    }
    await sleep(100);
  }
}

// dialogText is the text of a dialog Thunderbird has open ("Login to
// server ... failed", a question, an error after sending), or null.
function dialogText() {
  for (const w of Services.wm.getEnumerator(null)) {
    if (w.location.href == "chrome://global/content/commonDialog.xhtml" && w.document.readyState == "complete") {
      const text = [w.document.title, ...[...w.document.querySelectorAll("description")].map(e => e.textContent)]
        .map(t => t.trim()).filter(Boolean);
      const buttons = [...w.document.querySelector("dialog").shadowRoot.querySelectorAll("button")]
        .filter(b => !b.hidden).map(b => b.label).filter(Boolean);
      return [...new Set(text)].join(" | ") + " [" + buttons.join("] [") + "]";
    }
  }
  return null;
}

// closeDialogs answers every open dialog as its Cancel button does.
function closeDialogs() {
  let n = 0;
  for (const w of Services.wm.getEnumerator(null)) {
    if (w.location.href == "chrome://global/content/commonDialog.xhtml") {
      w.document.querySelector("dialog").cancelDialog();
      n++;
    }
  }
  return n;
}

// watchDialogs rejects as soon as a dialog is open: a test's script loses
// the race against it and the test hears what Thunderbird asks.
function watchDialogs(finished) {
  return new Promise((resolve, reject) => {
    const iv = win.setInterval(() => {
      const text = finished.done ? null : dialogText();
      if (finished.done || text) {
        win.clearInterval(iv);
      }
      if (text) {
        reject(new Error("DIALOG " + text));
      }
    }, 150);
  });
}

// url runs something that takes an nsIUrlListener and waits for its end.
function url(start, what) {
  return new Promise((resolve, reject) => {
    win.setTimeout(() => reject(new Error("no answer in 60 s" + (what ? " to " + what : ""))), 60000);
    start({
      QueryInterface: ChromeUtils.generateQI(["nsIUrlListener"]),
      OnStartRunningUrl() {},
      OnStopRunningUrl(u, status) {
        if (succeeded(status)) {
          resolve();
        } else {
          reject(new Error("the server operation failed: 0x" + (status >>> 0).toString(16)));
        }
      },
    });
  });
}

// copied runs something that takes an nsIMsgCopyServiceListener.
function copied(start) {
  return new Promise((resolve, reject) => {
    win.setTimeout(() => reject(new Error("the copy did not end in 60 s")), 60000);
    const done = status => {
      if (succeeded(status)) {
        resolve();
      } else {
        reject(new Error("the copy failed: 0x" + (status >>> 0).toString(16)));
      }
    };
    start({
      QueryInterface: ChromeUtils.generateQI(["nsIMsgCopyServiceListener"]),
      onStartCopy() {},
      onProgress() {},
      setMessageKey() {},
      getMessageId() {
        return null;
      },
      onStopCopy: done,
    });
  });
}

function imapFolder(f) {
  return f.QueryInterface(Ci.nsIMsgImapMailFolder);
}

// path is the folder's name on the server.
function path(f) {
  return f.getFlag(Flags.Inbox) ? "INBOX" : imapFolder(f).onlineName;
}

function special(f) {
  const out = [];
  for (const [name, flag] of [["inbox", Flags.Inbox], ["drafts", Flags.Drafts], ["sent", Flags.SentMail],
    ["junk", Flags.Junk], ["trash", Flags.Trash], ["archive", Flags.Archive], ["templates", Flags.Templates]]) {
    if (f.getFlag(flag)) {
      out.push(name);
    }
  }
  return out;
}

function folder(p) {
  const f = root.descendants.find(x => path(x) == p);
  if (!f) {
    throw new Error("Thunderbird has no folder " + p + "; it has " + root.descendants.map(path).join(", "));
  }
  return f;
}

// login stores the passwords for the IMAP and the SMTP server, as the
// password dialog does with "use Password Manager to remember".
async function login(password) {
  // What the servers remember of the old password goes first:
  // forgetPassword also takes the stored login away.
  server.forgetSessionPassword(false);
  for (const s of MailServices.outgoingServer.servers) {
    s.forgetPassword();
  }
  await Services.logins.removeAllUserFacingLoginsAsync();
  for (const scheme of ["imap", "smtp"]) {
    const origin = scheme + "://" + host;
    const li = Cc["@mozilla.org/login-manager/loginInfo;1"].createInstance(Ci.nsILoginInfo);
    li.init(origin, null, origin, server.username, password, "", "");
    await Services.logins.addLoginAsync(li);
  }
}

// discover logs in and asks for the list of folders, as starting
// Thunderbird with "check for new messages at startup" does.
async function discover() {
  await url(l => MailServices.imap.discoverAllFolders(root, l, msgWindow));
}

// update selects a folder on the server and brings its message list up to
// date, as clicking on it does. (The folder's own updateFolderWithListener
// forgets the listener when the folder is busy with something else.)
async function update(p) {
  const f = folder(p);
  await url(l => MailServices.imap.selectFolder(f, l, msgWindow));
  return f;
}

// sync discovers the folders and updates every one of them.
async function sync() {
  await discover();
  for (const f of root.descendants) {
    if (!f.getFlag(Flags.ImapNoselect)) {
      await url(l => MailServices.imap.selectFolder(f, l, msgWindow));
    }
  }
}

function folders() {
  return root.descendants.map(f => ({
    path: path(f),
    name: f.name,
    special: special(f),
    total: f.getTotalMessages(false),
    unread: f.getNumUnread(false),
  })).sort((a, b) => (a.path < b.path ? -1 : 1));
}

function describe(h) {
  return {
    uid: h.messageKey,
    id: h.messageId,
    subject: h.mime2DecodedSubject,
    from: h.mime2DecodedAuthor,
    to: h.mime2DecodedRecipients,
    size: h.messageSize,
    read: h.isRead,
    flagged: h.isFlagged,
    answered: !!(h.flags & MsgFlags.Replied),
    forwarded: !!(h.flags & MsgFlags.Forwarded),
    deleted: !!(h.flags & MsgFlags.IMAPDeleted),
    attachment: !!(h.flags & MsgFlags.Attachment),
    keywords: h.getStringProperty("keywords").split(" ").filter(Boolean).sort(),
  };
}

// messages is the message list of a folder as Thunderbird has it now,
// without asking the server.
function messages(p) {
  return [...folder(p).messages].map(describe).sort((a, b) => a.uid - b.uid);
}

function header(p, id) {
  const h = [...folder(p).messages].find(x => x.messageId == id);
  if (!h) {
    throw new Error("no message " + id + " in " + p);
  }
  return h;
}

function headers(p, ids) {
  return ids.map(id => header(p, id));
}

// source fetches a message from the server and returns it as it came, in
// base64, as opening it does.
function source(p, id) {
  const h = header(p, id);
  const uri = h.folder.getUriForMsg(h);
  return new Promise((resolve, reject) => {
    let data = "";
    MailServices.messageServiceFromURI(uri).streamMessage(uri, {
      QueryInterface: ChromeUtils.generateQI(["nsIStreamListener", "nsIRequestObserver"]),
      onStartRequest() {},
      onDataAvailable(req, stream, offset, count) {
        const s = Cc["@mozilla.org/scriptableinputstream;1"].createInstance(Ci.nsIScriptableInputStream);
        s.init(stream);
        data += s.readBytes(count);
      },
      onStopRequest(req, status) {
        if (succeeded(status)) {
          resolve(btoa(data));
        } else {
          reject(new Error("fetching the message failed: 0x" + (status >>> 0).toString(16)));
        }
      },
    }, msgWindow, null, false, "");
  });
}

// ---- changes, each as the menu command of the same name makes it ----

function markRead(p, ids, read) {
  folder(p).markMessagesRead(headers(p, ids), read);
}

function markFlagged(p, ids, flagged) {
  folder(p).markMessagesFlagged(headers(p, ids), flagged);
}

// tag sets or clears a keyword; Thunderbird's own tags are $label1 to
// $label5 ("Important" is $label1).
function tag(p, ids, keyword, on) {
  const f = folder(p);
  if (on) {
    f.addKeywordsToMessages(headers(p, ids), keyword);
  } else {
    f.removeKeywordsFromMessages(headers(p, ids), keyword);
  }
}

// transfer moves or copies messages to another folder.
async function transfer(from, ids, to, move) {
  const src = folder(from);
  const dst = folder(to);
  const list = headers(from, ids);
  await copied(l => MailServices.copy.copyMessages(src, list, dst, move, l, msgWindow, false));
}

// remove deletes messages as the Delete key does: to Trash, or for good
// when they are in Trash already.
async function remove(p, ids) {
  const f = folder(p);
  const list = headers(p, ids);
  await copied(l => f.deleteMessages(list, msgWindow, false, false, l, false));
}

async function emptyTrash() {
  await url(l => root.emptyTrash(l));
}

// compact is "Compact" on a folder: for IMAP an EXPUNGE.
async function compact(p) {
  const f = folder(p);
  await url(l => f.compact(l, msgWindow));
}

async function createFolder(parent, name) {
  const p = parent ? folder(parent) : root;
  const want = (parent ? parent + "/" : "") + name;
  p.createSubfolder(name, msgWindow);
  await until("the folder " + want, 20000, () => root.descendants.some(x => path(x) == want && imapFolder(x).verifiedAsOnlineFolder));
}

async function renameFolder(p, name) {
  const f = folder(p);
  const want = p.replace(/[^/]*$/, name);
  f.rename(name, msgWindow);
  await until("the folder " + want, 20000, () => root.descendants.some(x => path(x) == want));
}

// deleteFolder is "Delete" on a folder: Thunderbird moves it into Trash,
// or removes it when it is there already.
async function deleteFolder(p) {
  const f = folder(p);
  f.deleteSelf(msgWindow);
  await until("the folder " + p + " to go", 20000, () => !root.descendants.some(x => path(x) == p));
}

// ---- the compose window ----

// composeWindow waits for the compose window that open() asks for.
async function composeWindow(open) {
  const before = new Set(Services.wm.getEnumerator("msgcompose"));
  open();
  return until("the compose window", 30000, () => {
    for (const w of Services.wm.getEnumerator("msgcompose")) {
      if (!before.has(w) && w.gMsgCompose && w.composeEditorReady) {
        return w;
      }
    }
    return null;
  });
}

// compose opens a window for a new message in plain text: {to, cc,
// subject, body, attachments: [{path, name}]}, the paths in the container.
function compose(msg) {
  const fields = Cc["@mozilla.org/messengercompose/composefields;1"].createInstance(Ci.nsIMsgCompFields);
  fields.to = msg.to || "";
  fields.cc = msg.cc || "";
  fields.subject = msg.subject || "";
  fields.body = msg.body || "";
  for (const a of msg.attachments || []) {
    const att = Cc["@mozilla.org/messengercompose/attachment;1"].createInstance(Ci.nsIMsgAttachment);
    att.url = "file://" + a.path;
    att.name = a.name;
    fields.addAttachment(att);
  }
  const params = Cc["@mozilla.org/messengercompose/composeparams;1"].createInstance(Ci.nsIMsgComposeParams);
  params.type = Ci.nsIMsgCompType.New;
  params.format = Ci.nsIMsgCompFormat.PlainText;
  params.identity = account.defaultIdentity;
  params.composeFields = fields;
  return composeWindow(() => MailServices.compose.OpenComposeWindowWithParams(null, params));
}

// sendNow is the Send button: the window closes when the message was
// handed to the SMTP server and its copy was filed.
async function sendNow(w) {
  await sleep(300);
  w.goDoCommand("cmd_sendNow");
  await until("the compose window to close after sending", 60000, () => w.closed);
}

async function send(msg) {
  await sendNow(await compose(msg));
}

// reply is Reply on a message, with text typed above the quote.
async function reply(p, id, text) {
  const h = header(p, id);
  const uri = h.folder.getUriForMsg(h);
  const w = await composeWindow(() => MailServices.compose.OpenComposeWindow(null, h, uri,
    Ci.nsIMsgCompType.ReplyToSender, Ci.nsIMsgCompFormat.PlainText, account.defaultIdentity, null, msgWindow));
  w.GetCurrentEditor().insertText(text);
  await sendNow(w);
}

// saveDraft is File > Save > Draft in a compose window.
async function saveDraft(w) {
  w.SaveAsDraft();
  await sleep(200);
  await until("the draft to be saved", 30000, () => !w.gSaveOperationInProgress && !w.gSendOperationInProgress);
}

function setSubject(w, subject) {
  const field = w.document.getElementById("msgSubject");
  field.value = subject;
  field.dispatchEvent(new w.Event("input", { bubbles: true }));
}

// arrived waits for a message to show up in a folder's list without
// asking the server for anything: what a person sees while doing nothing.
function arrived(p, id, ms) {
  return until("the message " + id + " in " + p, ms, () => [...folder(p).messages].some(h => h.messageId == id));
}
