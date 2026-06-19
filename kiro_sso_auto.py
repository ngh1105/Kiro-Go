#!/usr/bin/env python3
"""
Kiro-Go Enterprise SSO (Microsoft 365) auto-login via Playwright.
Uses pure JS DOM manipulation to bypass Microsoft anti-bot detection.

Usage:
    python kiro_sso_auto.py
    python kiro_sso_auto.py --start 3 --count 1
    python kiro_sso_auto.py --debug
"""

import argparse
import re
import sys
import time

# Force UTF-8 on Windows
if sys.platform == "win32":
    import io
    sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")
    sys.stderr = io.TextIOWrapper(sys.stderr.buffer, encoding="utf-8", errors="replace")

from playwright.sync_api import sync_playwright, TimeoutError as PlaywrightTimeout

KIRO_ADMIN_URL = "http://localhost:8080/admin"
ADMIN_PASSWORD = "changeme"
DEFAULT_ACCOUNTS_FILE = r"C:\Users\Admin\Downloads\Telegram Desktop\7a7cb40c3aca4fad9bd6bca890aafde0.txt"
SSO_POLL_TIMEOUT = 60
SSO_POLL_INTERVAL = 2


def restart_kiro_server():
    """Kill all Kiro-Go processes and restart."""
    import subprocess, os
    # Kill all Go/main processes from Kiro-Go
    subprocess.run(
        'powershell -Command "Get-Process | Where-Object {$_.ProcessName -eq \'main\' -or $_.ProcessName -eq \'go\'} | Stop-Process -Force"',
        shell=True, capture_output=True,
    )
    time.sleep(3)
    # Start new server via go run
    subprocess.Popen(
        ['go', 'run', 'main.go'],
        cwd=r'C:\Users\Admin\Kiro-Go',
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        env={**os.environ, 'PATH': os.environ.get('PATH', '')},
    )
    time.sleep(8)
    # Verify
    import urllib.request
    for _ in range(15):
        try:
            r = urllib.request.urlopen(f'{KIRO_ADMIN_URL}', timeout=3)
            if r.status == 200:
                return True
        except Exception:
            time.sleep(1)
    return False


def parse_accounts(path: str) -> list[dict]:
    """Parse [account N] username: ... password: ... format."""
    accounts = []
    current = {}
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            if re.match(r"^\[account\s+\d+\]", line, re.IGNORECASE):
                if current and "username" in current and "password" in current:
                    accounts.append(current)
                current = {}
            elif line.startswith("username:"):
                current["username"] = line.split(":", 1)[1].strip()
            elif line.startswith("password:"):
                current["password"] = line.split(":", 1)[1].strip()
    if current and "username" in current and "password" in current:
        accounts.append(current)
    return accounts


def handle_microsoft_login(page, username: str, password: str, debug: bool = False):
    """Microsoft OAuth login using pure JS (bypass anti-bot)."""
    if debug:
        print(f"  [debug] MS URL: {page.url}")

    # Step 1: Email + Next
    print("  [*] Step 1: Email + Next (JS)")
    r1 = page.evaluate("""(username) => {
        const el = document.querySelector('input[type="email"]') ||
                   document.querySelector('input[name="loginfmt"]') ||
                   document.querySelector('input[name="login"]');
        if (!el) return 'NO_EMAIL';
        const s = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
        s.call(el, username);
        el.dispatchEvent(new Event('input', {bubbles: true}));
        el.dispatchEvent(new Event('change', {bubbles: true}));
        const btn = document.querySelector('#idSIButton9') ||
                    document.querySelector('input[type="submit"]') ||
                    document.querySelector('button[type="submit"]');
        if (btn) { btn.click(); return 'OK'; }
        const f = el.closest('form');
        if (f) { f.submit(); return 'FORM'; }
        return 'NO_BTN';
    }""", username)
    print(f"  [+] {r1}")

    time.sleep(5)
    if debug:
        print(f"  [debug] URL: {page.url}")

    # Step 2: Password + Sign in
    print("  [*] Step 2: Password + Sign in (JS)")
    r2 = page.evaluate("""(password) => {
        const el = document.querySelector('input[type="password"]') ||
                   document.querySelector('input[name="passwd"]') ||
                   document.querySelector('input[name="Password"]');
        if (!el) return 'NO_PW';
        const s = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
        s.call(el, password);
        el.dispatchEvent(new Event('input', {bubbles: true}));
        el.dispatchEvent(new Event('change', {bubbles: true}));
        const btn = document.querySelector('#idSIButton9') ||
                    document.querySelector('input[type="submit"]') ||
                    document.querySelector('button[type="submit"]');
        if (btn) { btn.click(); return 'OK'; }
        const f = el.closest('form');
        if (f) { f.submit(); return 'FORM'; }
        return 'NO_BTN';
    }""", password)
    print(f"  [+] {r2}")

    time.sleep(5)
    if debug:
        print(f"  [debug] URL: {page.url}")
        page.screenshot(path="debug_after_password.png")

    # Check if OAuth flow broke (MFA setup, bot detection, etc.)
    if "/login" in page.url.lower() and "oauth" not in page.url.lower():
        body = page.evaluate("() => document.body ? document.body.innerText.slice(0, 500) : ''")
        if debug:
            print(f"  [debug] MS /login page body: {body[:300]}")

        # Handle MFA setup: "keep your account secure" → Next → Skip
        if "keep your account secure" in body.lower() or "verify it" in body.lower():
            print("  [*] MFA setup detected — trying Next → Skip...")

            # Click Next
            next_clicked = page.evaluate("""() => {
                const btns = document.querySelectorAll('input[type="submit"], button[type="submit"], button');
                for (const b of btns) {
                    const t = (b.textContent || b.value || '').trim().toLowerCase();
                    if (t === 'next') { b.click(); return 'OK'; }
                }
                return 'NO_NEXT';
            }""")
            print(f"  [+] MFA Next: {next_clicked}")
            time.sleep(8)  # Wait for MFA page to fully render

            # Screenshot for debug
            if debug:
                page.screenshot(path="debug_mfa_page.png")
                print(f"  [debug] MFA page screenshot saved")

            # MFA page — try multiple approaches to click Skip
            try:
                page.wait_for_load_state("networkidle", timeout=15_000)
                time.sleep(3)
                print("  [*] Searching for Skip...")

                # Try every possible approach
                skipped = False
                approaches = [
                    # By role
                    lambda: page.get_by_role("link", name="Skip").first.click(timeout=3000),
                    lambda: page.get_by_role("link", name="Skip for now").first.click(timeout=3000),
                    lambda: page.get_by_role("button", name="Skip").first.click(timeout=3000),
                    # By text
                    lambda: page.get_by_text("Skip for now").first.click(timeout=3000),
                    lambda: page.get_by_text("Skip setup").first.click(timeout=3000),
                    lambda: page.get_by_text("Skip").first.click(timeout=3000),
                    lambda: page.locator('text="I want to set up later"').first.click(timeout=3000),
                    # By CSS
                    lambda: page.locator('a:has-text("Skip")').first.click(timeout=3000),
                    lambda: page.locator('button:has-text("Skip")').first.click(timeout=3000),
                    # "Other options" first then Skip
                    lambda: page.locator('text="Other options"').first.click(timeout=3000),
                ]
                for i, approach in enumerate(approaches):
                    try:
                        approach()
                        print(f"  [+] MFA: approach {i} worked")
                        if i == 9:  # Clicked "Other options"
                            time.sleep(3)
                            # Now find Skip
                            for j, a2 in enumerate(approaches[:8]):
                                try:
                                    a2()
                                    print(f"  [+] MFA Skip: approach {j}")
                                    skipped = True
                                    break
                                except: pass
                        else:
                            skipped = True
                        if skipped:
                            break
                    except Exception:
                        pass

                if not skipped:
                    print("  [*] MFA: Skip not found — waiting for timeout resolution")
            except Exception as e:
                print(f"  [*] MFA: {e}")
            time.sleep(4)

            if debug:
                print(f"  [debug] After MFA handling: {page.url}")
                body2 = page.evaluate("() => document.body ? document.body.innerText.slice(0, 300) : ''")
                print(f"  [debug] Body: {body2[:200]}")
        else:
            print(f"  [!] OAuth broken. Body: {body[:200]}")

    # Step 3: "Stay signed in?" → Skip
    print("  [*] Step 3: Stay signed in? (JS)")
    r3 = page.evaluate("""() => {
        const btns = document.querySelectorAll('button, input[type="submit"], input[type="button"]');
        for (const b of btns) {
            const t = (b.textContent || b.value || '').trim().toLowerCase();
            if (t === 'no' || t === 'skip' || t === 'not now' || t.includes("dont show")) {
                b.click(); return 'CLICKED_' + t.slice(0, 20);
            }
        }
        const body = document.body ? document.body.innerText.slice(0, 300) : '';
        return body.includes('Stay signed') ? 'KMSI' : 'NONE';
    }""")
    print(f"  [+] {r3}")

    return True


def process_account(context, account: dict, index: int, headless: bool, debug: bool):
    """Login one account via Enterprise SSO into Kiro-Go."""
    # Fresh page per account to avoid Microsoft session caching
    page = context.new_page()
    username = account["username"]
    password = account["password"]
    print(f"\n{'='*60}")
    print(f"Account {index}: {username}")
    print(f"{'='*60}")

    # 1. Go to Kiro-Go admin
    print("  [1] Opening Kiro-Go admin...")
    page.goto(KIRO_ADMIN_URL, wait_until="domcontentloaded", timeout=15_000)
    time.sleep(1)

    # 2. Login if needed
    if page.locator("#loginPage").is_visible(timeout=2_000):
        print("  [2] Logging into admin panel...")
        page.locator("#pwdField").fill(ADMIN_PASSWORD)
        page.locator("#loginBtn").click()
        time.sleep(2)

    # 3. Accounts tab
    try:
        tab = page.locator('.tab[data-tab="accounts"]')
        if tab.is_visible(timeout=2_000):
            tab.click()
            time.sleep(1)
    except Exception:
        pass

    # 4. Add Account
    print("  [3] Opening Add Account modal...")
    page.wait_for_selector(
        'button:has-text("Add Account"), button:has-text("添加账号")',
        timeout=5_000,
    ).click()
    time.sleep(1)

    # 5. Enterprise SSO
    print("  [4] Selecting Enterprise SSO...")
    page.wait_for_selector('.method-card[data-method="enterprisesso"]', timeout=5_000).click()
    time.sleep(1)

    # 6. Start SSO login — click button, then read signInUrl from DOM
    print("  [5] Starting SSO login...")
    page.wait_for_selector('#startKiroSsoBtn', timeout=8_000).click()

    # Wait for the API response to populate the DOM (up to 8 seconds)
    signin_url = None
    for attempt in range(40):  # 40 × 0.2s = 8s
        time.sleep(0.2)
        try:
            txt = page.locator("#kiroSsoSignInUrl").text_content()
            if txt and txt.strip().startswith("http"):
                signin_url = txt.strip()
                break
        except Exception:
            pass
        # Also check for error toast
        try:
            toast = page.locator(".toast").text_content()
            if toast and ("error" in toast.lower() or "fail" in toast.lower() or "bind" in toast.lower()):
                print(f"  [debug] Toast: {toast[:100]}")
        except Exception:
            pass

    if not signin_url:
        # Fallback: try API intercept
        captured = {"v": None}
        def on_resp(response):
            if "/auth/kiro-sso/start" in response.url:
                try:
                    d = response.json()
                    if d.get("signInUrl"):
                        captured["v"] = d["signInUrl"]
                    elif d.get("error") and debug:
                        print(f"  [debug] SSO API error: {d['error'][:100]}")
                except Exception:
                    pass
        page.on("response", on_resp)
        page.locator('#startKiroSsoBtn').click()
        for _ in range(25):
            if captured["v"]:
                break
            time.sleep(0.2)
        page.remove_listener("response", on_resp)
        signin_url = captured["v"]

    if not signin_url:
        print("  [FAIL] Cannot get signInUrl — restart Kiro-Go and retry")
        return False

    # 7. Kiro portal → Microsoft login
    print("  [6] Kiro portal → Microsoft...")
    sso_page = page.context.new_page()

    if debug:
        def log_nav(req):
            if req.is_navigation_request():
                print(f"  [debug] Nav: {req.url[:120]}")
        sso_page.on("request", log_nav)

    sso_page.goto(signin_url, wait_until="domcontentloaded", timeout=30_000)
    time.sleep(4)

    # 7a. Click "Your organization"
    print("  [7] Clicking 'Your organization'...")
    try:
        sso_page.locator('button:has-text("Your organization"):visible').first.click(timeout=5_000)
        print("  [+] OK")
        time.sleep(3)
    except Exception as e:
        print(f"  [*] {e}")

    # 7b. Enter email on Kiro sign-in
    print("  [8] Entering email on Kiro...")
    try:
        inp = sso_page.wait_for_selector(
            'input[type="email"], input[type="text"][name="email"], '
            'input[name="username"], input[id*="email" i], '
            'input:not([type="checkbox"]):not([type="hidden"])',
            timeout=10_000,
        )
        inp.click()
        inp.fill(username)
        time.sleep(0.5)
        print(f"  [+] Filled: {username}")

        # Click Continue
        for txt in ["Continue", "Sign in", "Next"]:
            try:
                b = sso_page.locator(f'button:has-text("{txt}"):visible').first
                if b.is_visible(timeout=1_000):
                    b.click()
                    print(f"  [+] Clicked '{txt}'")
                    break
            except Exception:
                pass
        else:
            inp.press("Enter")
        time.sleep(3)
        if debug:
            sso_page.screenshot(path="debug_after_kiro_continue.png")
            print(f"  [debug] Screenshot saved — after Kiro Continue")
        time.sleep(5)
    except PlaywrightTimeout:
        print("  [*] No email input on Kiro — redirecting directly")
    except Exception as e:
        print(f"  [*] {e}")

    if debug:
        print(f"  [debug] URL after Kiro: {sso_page.url}")

    # Wait for redirect chain: Kiro → localhost:3128 → Microsoft
    # The redirect goes: app.kiro.dev → localhost:3128/signin/callback → login.microsoftonline.com
    print("  [*] Waiting for redirect to Microsoft...")
    ms_reached = False
    for _ in range(60):  # up to 60 seconds
        time.sleep(1)
        current = sso_page.url.lower()
        if any(h in current for h in ["login.microsoftonline.com", "login.live.com", "login.windows.net"]):
            ms_reached = True
            break
        if "sign-in complete" in current or ("localhost" in current and "oauth/callback" in current):
            print("  [*] Reached callback page directly")
            break
        if debug and _ % 5 == 0:
            print(f"  [debug] Waiting... ({_+1}s) URL: {sso_page.url[:100]}")

    # 8. Microsoft login
    if ms_reached or any(h in sso_page.url.lower() for h in ["login.microsoftonline.com", "login.live.com"]):
        print("  [9] Microsoft login page reached!")
        handle_microsoft_login(sso_page, username, password, debug=debug)
        time.sleep(5)
    else:
        print(f"  [*] Not on MS login page: {sso_page.url[:120]}")

    # 9. Close SSO tab, wait for poll
    sso_page.close()
    page.bring_to_front()

    print("  [10] Waiting for SSO poll...")
    for attempt in range(SSO_POLL_TIMEOUT // SSO_POLL_INTERVAL):
        time.sleep(SSO_POLL_INTERVAL)
        if not page.locator(".modal.active").is_visible(timeout=500):
            print(f"  [+] Modal closed — account added!")
            return True
        if attempt > 0 and attempt % 10 == 0:
            print(f"  [*] Still waiting... ({attempt * SSO_POLL_INTERVAL}s)")

    print("  [?] Poll timeout")
    return False


def main():
    parser = argparse.ArgumentParser(description="Kiro-Go Enterprise SSO auto-login")
    parser.add_argument("--accounts", default=DEFAULT_ACCOUNTS_FILE)
    parser.add_argument("--start", type=int, default=1)
    parser.add_argument("--count", type=int, default=0)
    parser.add_argument("--headless", action="store_true")
    parser.add_argument("--debug", action="store_true")
    args = parser.parse_args()

    accounts = parse_accounts(args.accounts)
    if not accounts:
        print(f"ERROR: No accounts found in {args.accounts}")
        sys.exit(1)

    print(f"Loaded {len(accounts)} accounts")
    start_idx = max(0, args.start - 1)
    if args.count > 0:
        accounts = accounts[start_idx : start_idx + args.count]
    else:
        accounts = accounts[start_idx:]

    print(f"Processing {len(accounts)} accounts (starting #{args.start})")
    print(f"Debug: {'ON' if args.debug else 'OFF'}")

    results = {"ok": 0, "fail": 0}

    with sync_playwright() as pw:
        browser = pw.chromium.launch(
            headless=args.headless,
            args=[
                '--disable-blink-features=AutomationControlled',
                '--disable-features=IsolateOrigins,site-per-process',
            ],
        )
        context = browser.new_context(
            viewport={"width": 1280, "height": 800},
            locale="en-US",
            user_agent=(
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                "AppleWebKit/537.36 (KHTML, like Gecko) "
                "Chrome/125.0.0.0 Safari/537.36"
            ),
        )
        # Contexts created per-account in the loop below

        for i, account in enumerate(accounts):
            idx = start_idx + i + 1
            # Fresh incognito context per account (prevents Microsoft session caching)
            ctx = browser.new_context(
                viewport={"width": 1280, "height": 800},
                locale="en-US",
                user_agent=(
                    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                    "AppleWebKit/537.36 (KHTML, like Gecko) "
                    "Chrome/125.0.0.0 Safari/537.36"
                ),
            )
            ctx.add_init_script("""
                Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
                Object.defineProperty(navigator, 'plugins', {get: () => [1, 2, 3, 4, 5]});
                window.chrome = { runtime: {} };
            """)
            try:
                ok = process_account(ctx, account, idx, args.headless, args.debug)
                results["ok" if ok else "fail"] += 1
            except Exception as e:
                print(f"  [FAIL] {e}")
                if args.debug:
                    import traceback
                    traceback.print_exc()
                results["fail"] += 1
            finally:
                ctx.close()

        browser.close()

    print(f"\n{'='*60}")
    print(f"DONE: {results['ok']} OK, {results['fail']} FAIL")
    print(f"{'='*60}")


if __name__ == "__main__":
    main()
