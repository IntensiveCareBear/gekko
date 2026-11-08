#!/bin/bash
set -e

echo "=== Ollama Model Initialization Script ==="

# 1. Verify Ollama is running
if ! curl -s http://localhost:11434/api/tags > /dev/null; then
    echo "[ERROR] Ollama is not running on http://localhost:11434."
    echo "Please start the Ollama application or run 'ollama serve' in another terminal."
    exit 1
fi
echo "[OK] Ollama server is active."

# Helper to check and pull a model
pull_model_if_missing() {
    local model=$1
    echo "Checking if model '$model' is installed..."
    # Match exact model name (or name with tag) in the list output
    if ollama list | grep -F -w "$model" >/dev/null 2>&1; then
        echo "[OK] Model '$model' is already installed."
    else
        echo "Pulling model '$model'..."
        ollama pull "$model"
        echo "[OK] Successfully pulled '$model'."
    fi
}

# 2. Pull nomic-embed-text (all RAG embeddings; the Qdrant collections are built
# in this vector space, so it is never substitutable)
pull_model_if_missing "nomic-embed-text"

# 3. Pull qwen2.5:7b (fleet-default proposal model)
pull_model_if_missing "qwen2.5:7b"

# 3b. Pull llama3.3:70b (larger proposal model; run.sh default via OLLAMA_MODEL)
pull_model_if_missing "llama3.3:70b"

# 4. Pull qwen2.5:1.5b (fast validation parser fallback + Assistant chat model)
pull_model_if_missing "qwen2.5:1.5b"

# 5. Pull gemma3:4b (gemma-neutral persona Model override)
pull_model_if_missing "gemma3:4b"

echo "=== All models initialized successfully! ==="
