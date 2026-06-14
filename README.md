# 🦀 Claude Proxy Pro

![Claude Proxy Pro Icon](1781476674.png)

> **"You won't have to sell your kidney to use Claude Code anymore! hahaha"** 💸

**Claude Proxy Pro** is an ultra-lightweight, blazing-fast, and standalone desktop application built entirely in **Go** and **Wails**. It serves as an invisible, highly intelligent bridge between Anthropic's amazing [Claude Code](https://github.com/anthropics/claude-code) CLI and **ANY** LLM provider of your choice (OpenRouter, Groq, Ollama, etc.).

## 🚨 The Problem: Claude Code is Expensive!
Recent news and developer reports have highlighted that while Claude Code is arguably the most powerful agentic coding tool available, its heavy reliance on autonomous "thinking" and "tool-use" loops consumes tokens at a terrifying rate. Active developers are reporting API bills upwards of **$150 to $250+ per month**. 

## 🦸‍♂️ The Solution: Claude Proxy Pro
Instead of relying on heavy Python or Node.js scripts that eat up your RAM and require constant terminal wrangling, we built a native, standalone proxy app. 

With **Claude Proxy Pro**, you can instantly route your Claude Code traffic to cheaper (or free!) models like `qwen3-235b`, `llama-3.3`, or `mixtral` while keeping the flawless Claude Code terminal experience exactly the same.

### ⚡ Why is it Legendary?
- **Written in pure Go:** Consumes a microscopic **~88MB of RAM**. (Goodbye 500MB+ Electron apps!)
- **Sleek Glassmorphism UI:** A gorgeous, native macOS-style dashboard to manage your providers and models.
- **Auto-Syncs with Claude Code:** The moment you select a model in the UI, the proxy automatically injects it into your `~/.claude/settings.json`. Zero manual configuration required.
- **Failover & Auto-Retry:** If a provider goes down, the Stability Engine seamlessly retries or routes to a backup node without breaking your Claude Code session.
- **Live Hacker Terminal:** Watch your proxy route traffic in real-time with our built-in Matrix-style live system logs.

## 🛠 Installation
No Node.js. No Python. No dependencies.
Just head over to the [Releases Page](../../releases) and download the pre-compiled version for your system:
- **macOS:** Download the `.app.zip`, extract it, and drag it to Applications.
- **Windows:** Download the `.exe` and run it.
- **Linux:** Download the binary and execute it.

*(Works seamlessly on macOS, Windows, and Linux)*

## 🎮 How to use
1. Open **Claude Proxy Pro**.
2. Go to the **Providers** tab and add your favorite provider (e.g., OpenRouter) and your API Key.
3. Click **Sync Models** on the Models tab.
4. Pick the model you want and hit **Activate**.
5. Open your terminal and run `claude`. That's it!

---
*Built with ❤️ (and a lot of coconut juice) for the open-source community.*
