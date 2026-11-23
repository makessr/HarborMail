"""Protocol compatibility fixture using independent Python mail clients."""

from email.message import EmailMessage
import imaplib
import json
import os
from pathlib import Path
import poplib
from queue import Empty, Queue
import re
import smtplib
import ssl
import subprocess
import sys
import tempfile
from threading import Thread
import time


def mail_fixture(sender, recipient, subject):
    message = EmailMessage()
    message["From"], message["To"], message["Subject"] = sender, recipient, subject
    message.set_content("Synthetic protocol fixture")
    return message


def ports_from_logs(process, protocols):
    lines = Queue()

    def read_logs():
        for line in process.stdout:
            lines.put(line)
        lines.put(None)

    Thread(target=read_logs, daemon=True).start()
    deadline = time.monotonic() + 20
    ports, seen = {}, []
    while len(ports) < len(protocols):
        try:
            line = lines.get(timeout=max(0, deadline - time.monotonic()))
        except Empty as err:
            raise RuntimeError("Harbor Mail listeners did not start: " + "".join(seen)) from err
        if line is None:
            raise RuntimeError("Harbor Mail stopped: " + "".join(seen))
        seen.append(line)
        match = re.search(r"(.*?) listening on 127\.0\.0\.1:(\d+)", line.strip())
        if match:
            for name, label in protocols.items():
                if match[1].endswith(label):
                    ports[name] = int(match[2])
    return ports


def verify(binary, directory):
    sender, recipient = "sender@harbor.test", "recipient@harbor.test"
    password = "synthetic-client-password"
    digest = subprocess.check_output([binary, "hash-password"], input=password + "\n", text=True).strip()
    cert, key = directory / "cert.pem", directory / "key.pem"
    subprocess.run(
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
         "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
         "-keyout", key, "-out", cert],
        check=True, capture_output=True,
    )
    protocols = {
        "HARBOR_SMTP": "SMTP",
        "HARBOR_SUBMISSION": "SMTP submission (STARTTLS)",
        "HARBOR_SUBMISSIONS": "SMTP submission (implicit TLS)",
        "HARBOR_IMAPS": "IMAPS",
        "HARBOR_POP3S": "POP3S",
    }
    env = {key: value for key, value in os.environ.items() if not key.startswith("HARBOR_")}
    env.update({key: "127.0.0.1:0" for key in protocols})
    env.update(
        HARBOR_DOMAIN="harbor.test", HARBOR_ACCOUNTS=json.dumps({sender: digest, recipient: digest}),
        HARBOR_CERT=str(cert), HARBOR_KEY=str(key), HARBOR_DB=str(directory / "mail.db"),
        HARBOR_WEB="127.0.0.1:0", HARBOR_DIRECT_MX="false", HARBOR_PUBLIC_DEFAULTS="true",
    )
    process = subprocess.Popen(
        [binary], cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    try:
        ports = ports_from_logs(process, protocols)
        context = ssl.create_default_context(cafile=cert)
        host = "127.0.0.1"
        with smtplib.SMTP(host, ports["HARBOR_SMTP"], timeout=5) as client:
            client.ehlo()
            assert client.mail(sender)[0] == 250
            assert client.rcpt("missing@harbor.test")[0] == 550
            client.rset()
            assert client.send_message(mail_fixture(sender, recipient, "SMTP fixture")) == {}
        client = smtplib.SMTP(host, ports["HARBOR_SMTP"], timeout=5)
        try:
            client.ehlo()
            assert client.mail(sender)[0] == 250
            for _ in range(10):
                assert client.rcpt("missing@harbor.test")[0] == 550
            assert client.rcpt("missing@harbor.test")[0] == 421
        finally:
            client.close()
        with smtplib.SMTP(host, ports["HARBOR_SUBMISSION"], timeout=5) as client:
            client.starttls(context=context)
            client.login(sender, password)
            assert client.send_message(mail_fixture(sender, recipient, "STARTTLS fixture")) == {}
        with smtplib.SMTP_SSL(host, ports["HARBOR_SUBMISSIONS"], context=context, timeout=5) as client:
            client.login(sender, password)
            assert client.send_message(mail_fixture(sender, recipient, "SMTPS fixture")) == {}
        with imaplib.IMAP4_SSL(host, ports["HARBOR_IMAPS"], ssl_context=context, timeout=5) as client:
            assert client.login(recipient, password)[0] == "OK"
            assert client.select("INBOX")[1] == [b"3"]
            assert len(client.uid("search", None, "ALL")[1][0].split()) == 3
        client = poplib.POP3_SSL(host, ports["HARBOR_POP3S"], context=context, timeout=5)
        try:
            assert client.user(recipient).startswith(b"+OK")
            assert client.pass_(password).startswith(b"+OK")
            assert client.stat()[0] == 3
            assert b"Subject: SMTP fixture" in client.retr(1)[1]
            client.quit()
        finally:
            client.close()
        print("Independent clients passed: SMTP, STARTTLS SMTP AUTH, SMTPS, IMAPS and POP3S")
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="harbor-mail-clients-") as directory:
        verify(str(Path(sys.argv[1]).resolve()), Path(directory))
