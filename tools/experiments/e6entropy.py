import os, random, subprocess, sys, json, collections
CSV="/data/data/com.termux/files/home/diskreclaim2/files.csv"
OUT="/data/data/com.termux/files/home/e6entropy.json"
TEXT_EXT={".py",".js",".json",".txt",".conf",".cfg",".sh",".yaml",".yml",".xml",".md",".html",".css",".mjs",".ini",".list",".dist",".tmpl",".service",".svg",".csv",".ts",".jsx",".map"}
ARCH={".gz",".xz",".bz2",".zst",".zip",".tgz",".png",".jpg",".jpeg",".webp",".gif",".woff",".woff2",".ttf",".otf",".eot",".mp3",".mp4",".pdf",".deb",".jar"}
def fam(p):
    e=os.path.splitext(p)[1].lower()
    if e in TEXT_EXT: return "text"
    if e in ARCH: return "already_compressed"
    return "binary_other"
rows=[]
for line in open(CSV):
    q=line.rstrip("\n").split("\t")
    if len(q)>=3 and 4096<=int(q[1])<=2097152 and os.path.isfile(q[0]):
        rows.append((q[0],int(q[1]),fam(q[0])))
random.seed(7); random.shuffle(rows)
def zc(path,level):
    r=subprocess.run(["zstd","-%d"%level,"-q","-c",path],capture_output=True)
    return len(r.stdout) if r.returncode==0 and len(r.stdout)>0 else None
sample={}   # (fam,bucket) -> [ratios]
tested=0
for p,s,f in rows:
    if tested>=2400: break
    b="small" if s<65536 else "big"
    c=zc(p,3)
    if c is None: continue
    sample.setdefault((f,b),[]).append(c/s)
    tested+=1
allrows=collections.defaultdict(lambda:[0,0])  # fam -> [count, bytes] over ALL csv files
for line in open(CSV):
    q=line.rstrip("\n").split("\t")
    if len(q)>=3:
        f=fam(q[0]); allrows[f][0]+=1; allrows[f][1]+=int(q[1])
res={"sampled":tested,"families":{}}
for f in allrows:
    cnt,byts=allrows[f]
    fr=[]
    for b in ("small","big"):
        v=sample.get((f,b))
        if v: fr.append({"bucket":b,"n":len(v),"mean_ratio":round(sum(v)/len(v),4)})
    res["families"][f]={"corpus_files":cnt,"corpus_bytes":byts,"ratios":fr}
# corpus-wide estimate at zstd -3 using mean ratio per (fam,bucket), global mean fallback
gm=[]
for k,v in sample.items(): gm+=v
gmean=sum(gm)/len(gm)
est_raw=0; est_c=0; incompressible_b=0
for line in open(CSV):
    q=line.rstrip("\n").split("\t")
    if len(q)<3: continue
    s=int(q[1]); f=fam(q[0]); b="small" if s<65536 else "big"
    v=sample.get((f,b)); r=sum(v)/len(v) if v else gmean
    est_raw+=s; est_c+=s*r
    if r>=0.95: incompressible_b+=s
res["corpus_all_files_bytes"]=est_raw
res["est_zstd3_bytes"]=int(est_c)
res["est_zstd3_saved_bytes"]=int(est_raw-est_c)
res["near_incompressible_bytes"]=incompressible_b
json.dump(res,open(OUT,"w"),indent=1)
print(json.dumps(res,indent=1))
