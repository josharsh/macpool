import json, os, socket, sys, time
row = json.loads(sys.argv[1])
time.sleep(0.5)  # pretend this is a slow API call
row["shout"] = " ".join(v.upper() for k, v in sorted(row.items())) + "!"
row["ran_on"] = socket.gethostname() + " pid " + str(os.getpid())
print("working...", file=sys.stderr)
print(json.dumps(row))
