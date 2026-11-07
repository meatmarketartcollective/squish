import os, subprocess, sys, json
ST="/data/data/com.termux/files/home/diskreclaim2/sqfinal/st"
def zc(path,level):
    r=subprocess.run(["zstd","-%d"%level,"-q","-c",path],capture_output=True)
    return len(r.stdout) if r.returncode==0 else None
res={"manifests":{"files":0,"raw":0,"z3":0,"z19":0,"fails":0},
     "frames":{"files":0,"raw":0,"z3":0,"z19":0,"fails":0}}
for label,d in (("manifests",ST+"/manifests"),("frames",ST+"/frames")):
    for root,_,fs in os.walk(d):
        for fn in fs:
            p=os.path.join(root,fn); raw=os.path.getsize(p)
            a=zc(p,3); b=zc(p,19)
            r=res[label]; r["files"]+=1; r["raw"]+=raw
            if a is None or b is None: r["fails"]+=1; continue
            r["z3"]+=a; r["z19"]+=b
print(json.dumps(res,indent=1))
json.dump(res,open("/data/data/com.termux/files/home/e5meta.json","w"),indent=1)
