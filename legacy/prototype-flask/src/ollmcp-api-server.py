#!/usr/bin/env python3
"""
HTTP API server for ollmcp queries to OpenCost
Exposes POST /query endpoint that accepts natural language queries
Supports model selection via API
"""
import json
import subprocess
import tempfile
import os
import time
import re
import pexpect
from flask import Flask, request, jsonify
from pathlib import Path

app = Flask(__name__)

# Configuration
MCP_CONFIG = "/root/.config/ollmcp/servers.json"
SAFE_DEFAULT_MODEL = 'qwen2.5:0.5b'
MODEL_PATTERN = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$')

def get_default_model():
    env_default_model = os.environ.get('DEFAULT_MODEL')
    if env_default_model is None:
        return SAFE_DEFAULT_MODEL
    if MODEL_PATTERN.fullmatch(env_default_model):
        return env_default_model

    app.logger.warning(
        "Invalid DEFAULT_MODEL %r does not match required pattern; "
        "falling back to safe default %r",
        env_default_model,
        SAFE_DEFAULT_MODEL
    )
    return SAFE_DEFAULT_MODEL

DEFAULT_MODEL = get_default_model()
@app.route('/health', methods=['GET'])
def health():
    """Health check endpoint"""
    return jsonify({
        "status": "healthy",
        "default_model": DEFAULT_MODEL
    })

@app.route('/query', methods=['POST'])
def query():
    """
    POST /query
    Body: {
        "query": "show me allocation costs for last 2 days",
        "model": "qwen2.5:7b"  # Optional, defaults to qwen2.5:0.5b
    }
    Returns: {"result": "...", "success": true}
    """
    try:
        data = request.get_json()
        if not data or 'query' not in data:
            return jsonify({"error": "Missing 'query' field in request body"}), 400

        user_query = data['query']
        model = data.get('model', DEFAULT_MODEL)  # Use provided model or default
        if not isinstance(model, str) or not MODEL_PATTERN.fullmatch(model):
            return jsonify({"error": "Invalid 'model' format"}), 400

        child = pexpect.spawn(
            'ollmcp',
            args=['-j', MCP_CONFIG, '-m', model],
            timeout=300,  # 5 minutes for complex queries
            encoding='utf-8'
        )

       
        child.expect(r'❯', timeout=30)

        # First disable HIL (Human-in-the-Loop) for automated execution
        child.sendline('hil')
        time.sleep(2) 
        child.expect(r'❯', timeout=10)

        # Send the query
        child.sendline(user_query)

        index = child.expect([r'❯', r'\(y\):', pexpect.TIMEOUT], timeout=300)

        if index == 1:  # Got a confirmation request
            child.sendline('y')  # Confirm
            child.expect(r'❯', timeout=300)  # Wait for the actual prompt after answer

        # Send quit command
        child.sendline('quit')

        # Get all output
        child.expect(pexpect.EOF, timeout=10)
        stdout = child.before

        # Extract the meaningful response (filter out UI elements)
        import re

        # Remove ANSI escape codes
        ansi_escape = re.compile(r'\x1B(?:[@-Z\\-_]|\[[0-?]*[ -/]*[@-~])')
        stdout = ansi_escape.sub('', stdout)

        answer_match = re.search(r'📝 Answer \(Markdown\):\s*\n(.*?)(?=📝 Answer|qwen2\.5/.*?❯|Exiting|$)', stdout, re.DOTALL)

        if answer_match:
            result = answer_match.group(1).strip()
            # Remove any box drawing characters and extra whitespace
            result = re.sub(r'[─│╭╰├┤╮╯┬┴┼]', '', result)
            result = re.sub(r'\n\s*\n', '\n', result).strip()
        else:
            # Fallback: try to find any answer section
            answer_match = re.search(r'📝 Answer:\s*\n(.*?)(?=📝 Answer|qwen2\.5/.*?❯|Exiting|$)', stdout, re.DOTALL)
            if answer_match:
                result = answer_match.group(1).strip()
                # Remove any box drawing characters and extra whitespace
                result = re.sub(r'[─│╭╰├┤╮╯┬┴┼]', '', result)
                result = re.sub(r'\n\s*\n', '\n', result).strip()
            else:
                # Last resort: filter out UI elements line by line
                lines = stdout.split('\n')
                result_lines = []
                skip_patterns = [
                    '─', '│', '╭', '╰', '├', '┤',  # Box drawing
                    '❯', '🤖', '📝', '⚠️', '✓', '✗',  # Symbols
                    'Welcome to', 'Available Tools', 'Current model',
                    'Found server', 'Connecting', 'Successfully connected',
                    'HIL confirmations', 'Tool calls will proceed',
                    'TERM environment', 'Exiting', 'What would you like',
                    'qwen2.5/'
                ]

                for line in lines:
                    if any(pattern in line for pattern in skip_patterns):
                        continue
                    if line.strip():
                        result_lines.append(line.strip())

                result = '\n'.join(result_lines).strip()

        return jsonify({
            "success": True,
            "query": user_query,
            "result": result,
            "model": model
        })

    except subprocess.TimeoutExpired:
        return jsonify({"error": "Query timeout after 300 seconds"}), 504
    except pexpect.exceptions.TIMEOUT as e:
        return jsonify({"error": f"Query timeout after 300 seconds: {str(e)}"}), 504
    except Exception as e:
        return jsonify({"error": str(e)}), 500

@app.route('/models', methods=['GET'])
def list_models():
    """List available Ollama models"""
    try:
        result = subprocess.run(
            ['ollama', 'list'],
            capture_output=True,
            text=True,
            timeout=10
        )

        # Parse ollama list output
        models = []
        for line in result.stdout.split('\n')[1:]:  # Skip header
            if line.strip():
                parts = line.split()
                if parts:
                    models.append({
                        "name": parts[0],
                        "size": parts[1] if len(parts) > 1 else "unknown"
                    })

        return jsonify({
            "models": models,
            "default_model": DEFAULT_MODEL
        })
    except Exception as e:
        return jsonify({"error": str(e)}), 500

@app.route('/tools', methods=['GET'])
def list_tools():
    """List available MCP tools from the server"""
    try:
        # Run ollmcp to get the tool list from the welcome screen
        process = subprocess.Popen(
            ['ollmcp', '-j', MCP_CONFIG, '-m', DEFAULT_MODEL],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True
        )

        # Just quit immediately - the welcome screen shows all tools
        input_text = "quit\n"
        stdout, stderr = process.communicate(input=input_text, timeout=30)

        tools = []
        lines = stdout.split('\n')

        import re
        for line in lines:
            # Pattern: ✓ followed by opencost.tool_name
            tool_matches = re.findall(r'✓\s+(opencost\.\w+)', line)
            for tool_name in tool_matches:
                tools.append({
                    "name": tool_name.strip(),
                    "description": ""
                })

        return jsonify({
            "tools": tools,
            "mcp_server": "opencost",
            "count": len(tools)
        })

    except subprocess.TimeoutExpired:
        return jsonify({"error": "Timeout fetching tools from MCP server"}), 504
    except Exception as e:
        return jsonify({"error": str(e)}), 500

if __name__ == '__main__':
    # Run on all interfaces, port 8888
    app.run(host='0.0.0.0', port=8888, debug=False)
