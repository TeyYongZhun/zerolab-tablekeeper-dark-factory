# Factory setup (hybrid)

| Seat | Runs where | Harness | Model | Budget |
|---|---|---|---|---|
| Architect | WSL, `agents/architect.py` | OpenCode | `MiniMaxAI/MiniMax-M2.5` (Featherless) | Featherless credit |
| QA Lead | WSL, `agents/qa_lead.py` | OpenCode | `MiniMaxAI/MiniMax-M2.5` (Featherless) | Featherless credit |
| Developer | Band Desktop on Windows, local agent | Claude Code | `claude-sonnet-5-5`, medium effort | Claude Pro |

All three seats share **one result repository**. We used `C:\band-work\result`, which WSL sees
as `/mnt/c/band-work/result`. Choose a path without spaces.

In the commands below, `$FACTORY` is the WSL path of the folder holding this factory's files
(`mandates/`, `agents/`, `.env`), for example `/mnt/c/work/factory`. Keep `.env` there, never
inside the result repository.

Commands marked **[WSL]** run in the Ubuntu terminal. Commands marked **[PS]** run in Windows PowerShell.

---

## 1. Docker access (5 min)

**[WSL]**
```sh
sudo usermod -aG docker $USER
```
**[PS]**
```powershell
wsl --shutdown
```
Reopen Ubuntu, then run **[WSL]**:
```sh
docker run --rm hello-world        # must print "Hello from Docker!" without sudo
```

## 2. Kickoff repo and harness (15 min)

**[WSL]**
```sh
cd ~
git clone https://github.com/band-ai/dark-factory-wearedevs.git
python3 -m venv ~/venvs/factory
. ~/venvs/factory/bin/activate
pip install -r ~/dark-factory-wearedevs/harness/requirements.txt
pip install -r "$FACTORY/requirements.txt"            # band-sdk[opencode]
python -m playwright install --with-deps chromium     # asks for sudo
cd ~/dark-factory-wearedevs && python -m harness --help
```
If a package fails to install on your Python version, install Python 3.12
(`sudo apt install python3.12-venv`) and recreate the venv with `python3.12 -m venv ~/venvs/factory`.

## 3. Shared result repos (5 min)

**[WSL]**
```sh
mkdir -p /mnt/c/band-work/result /mnt/c/band-work/toy-result ~/band-work/checks
for r in result toy-result; do
  cd /mnt/c/band-work/$r
  git init -b main
  git config core.autocrlf false      # Windows and WSL git share this repo
  git config core.filemode false
  printf '* text=auto eol=lf\n' > .gitattributes
  git add .gitattributes && git commit -m "Repository setup"
done
```
`eol=lf` matters: if Windows git writes CRLF line endings into a Dockerfile or shell script,
the Linux container breaks. `toy-result` is for a rehearsal on the event's practice track.

## 4. OpenCode and Featherless (15 min)

**[WSL]**
```sh
curl -fsSL https://opencode.ai/install | bash
exec $SHELL                              # reload PATH
mkdir -p ~/.config/opencode
cat > ~/.config/opencode/opencode.json <<'EOF'
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "featherless": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Featherless AI",
      "options": {
        "baseURL": "https://api.featherless.ai/v1",
        "apiKey": "{env:FEATHERLESS_API_KEY}"
      },
      "models": {
        "MiniMaxAI/MiniMax-M2.5": { "limit": { "context": 65536, "output": 16384 } }
      }
    }
  }
}
EOF
```
This config lives in your home folder. **Never** put an `opencode.json` inside the result repo.
The 16K output limit lets the Architect paste a full specification into one handoff; the 64K
context cap keeps each call cheap.

Banana test **[WSL]**:
```sh
export FEATHERLESS_API_KEY=<your key>
mkdir -p ~/scratch && cd ~/scratch
opencode models | grep featherless
opencode run -m featherless/MiniMaxAI/MiniMax-M2.5 \
  "Create hello.txt containing the word banana, then run 'wc -c hello.txt'."
cat hello.txt                            # should print: banana
```
Passing this test is not enough on its own: a seat also has to call Band's messaging tool,
which takes three parameters. `deepseek-ai/DeepSeek-V4-Flash` passed the banana test but kept
dropping those parameters (see FACTORY.md), so test any other model with the smoke test in step 8.

## 5. Create the seats in Band (20 min)

**Architect and QA Lead** are external agents that you run yourself through the SDK
([Band docs](https://docs.band.ai/getting-started/connect-remote-agent.md)):
1. Open <https://app.band.ai/agents> and click **New Agent**. Choose **External Agent**.
2. Name it exactly **Architect**, give it the handle `architect` and a one-line description.
   Keep **Personal Registry Access** on, so the seats can find each other.
3. A popup shows the **API Key** once. Copy it into `.env` as `BAND_ARCHITECT_API_KEY` straight away.
4. Open the agent's settings page and copy the **Agent UUID** (bottom right) into `.env` as `BAND_ARCHITECT_AGENT_ID`.
5. Repeat for an agent named exactly **QA Lead** with handle `qa-lead`, using
   `BAND_QA_LEAD_API_KEY` and `BAND_QA_LEAD_AGENT_ID`.

**Developer** is a Claude Code seat that Band Desktop runs
([Band docs](https://docs.band.ai/band-desktop.md)). Install the Claude Code CLI first
(`irm https://claude.ai/install.ps1 | iex` in PowerShell, then `claude` and `/login`).
1. Install Band Desktop and choose **Sign in with browser**.
2. Under **Claude Code readiness**, let it install the `band` CLI and the `band-peer` plugin,
   then click **Recheck** until every check passes.
3. Choose **New local agent**, then **Claude Code**. Name it exactly **Developer**, handle
   `developer`, and choose `mandates/developer.md` as its **Role** file.
4. Runtime: model **sonnet**, reasoning effort **medium**, permission mode **Auto**, working
   directory `C:\band-work\result`. Click **Test runtime** and save.
5. Put the exact model id it reports in `mandates/developer.md` → `Model:`.

A room session keeps the working directory it started with. After changing the directory,
use a new room.

**Check the handles.** Band addresses agents as `@<owner>/<agent>`. Our mandates use
`@teyyongzhun/architect`, `@teyyongzhun/developer` and `@teyyongzhun/qa-lead`; replace
`teyyongzhun` with your own Band handle prefix in all three mandates.

## 6. Fill in .env

Copy `.env.example` to `$FACTORY/.env` and fill it in, with no spaces around `=` and no quotes:
```
FEATHERLESS_API_KEY=<key>
RESULT_REPO=/mnt/c/band-work/result
BAND_ARCHITECT_AGENT_ID=...
BAND_ARCHITECT_API_KEY=...
BAND_QA_LEAD_AGENT_ID=...
BAND_QA_LEAD_API_KEY=...
```
Use `/mnt/c/band-work/toy-result` for a rehearsal.

## 7. Start the factory (3 terminals)

**[WSL] terminal 1**: the OpenCode server (keep it open)
```sh
cd "$FACTORY"
set -a; . <(tr -d "\r" < .env); set +a      # strips Windows line endings
opencode serve --hostname=127.0.0.1 --port=4096
```
**[WSL] terminal 2**: Architect
```sh
. ~/venvs/factory/bin/activate
cd "$FACTORY" && python agents/architect.py
```
**[WSL] terminal 3**: QA Lead
```sh
. ~/venvs/factory/bin/activate
cd "$FACTORY" && python agents/qa_lead.py
```
Each seat prints its model and working directory when it starts, for example
`architect: model MiniMaxAI/MiniMax-M2.5, working in /mnt/c/band-work/result`. The seat scripts
read `.env` only at start, so restart them after changing it. The Developer runs inside Band
Desktop.

## 8. Smoke test (10 min)

Create a test room and add **Architect**, **Developer** and **QA Lead**. Tag only the Architect:
> @Architect send a message to QA Lead, tagging them, asking which folder it is working in. Ask QA Lead to reply to you with a tag.

Both must reply with real tags (coloured chips), within a minute or two, without looping. This
exercises the messaging tool and the reciprocal `@handle` exchange that gate 2 checks.

## Running a stage

- Use a **new room** and a fresh result repository (only the `.gitattributes` commit).
- Disable sleep and start the screen recording.
- Send `@Architect` followed by the stage's task. The files in [`dispatch/`](dispatch/) are the
  exact tasks we sent: environment paths, the evaluation command, and the full specification.
- Send nothing else to the room until the Architect's final report. A long step on the
  OpenCode seats can take 10–20 minutes with no new message.
- Verify each stage yourself afterwards, without posting to the room:
  `cd ~/dark-factory-wearedevs && ~/venvs/factory/bin/python -m harness run --track <track> --repo /mnt/c/band-work/result --stage N --mode isolated --out ~/band-work/checks/<new-name>`
