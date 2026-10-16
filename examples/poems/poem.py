import json, sys
import anthropic

row = json.loads(sys.argv[1])
words = [row[c] for c in ("w1", "w2", "w3", "w4", "w5")]
client = anthropic.Anthropic()
msg = client.messages.create(
    model="claude-sonnet-5-5",
    max_tokens=300,
    messages=[{
        "role": "user",
        "content": f"Write a six-line poem in the style of a sonnet that uses all of these words: {', '.join(words)}. Output only the poem.",
    }],
)
row["poem"] = msg.content[0].text.strip()
print(json.dumps(row))
