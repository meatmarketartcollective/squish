import os, random, subprocess, sys, json, collections
CSV="/data/data/com.termux/files/home/diskreclaim2/files.csv"
OUT="/data/data/com.termux/files/home/e3sim.json"
TMP="/data/data/com.termux/files/usr/tmp"
layers=collections.defaultdict(dict)   # layer -> {md5: [size,...]}
for line in open(CSV):
    q=line.rstrip("\n").split("\t")
    if len(q)>=3:
        parts=q[0].split("/")
        ly=parts[parts.index("layers")+1]
        layers[ly].setdefault(q[2],0)
        layers[ly][q[2]]+=int(q[1])
L=list(layers)
def jac(a,b):
    ia=set(a)&set(b)
    return len(ia)/len(set(a)|set(b)) if (len(a)|len(b)) else 0
pairs=[]
for i in range(len(L)):
    for j in range(i+1,len(L)):
        j_=jac(layers[L[i]],layers[L[j]])
        pairs.append(j_)
        pairs[-1]=(L[i],L[j],j_)
buckets=collections.Counter()
for _,_,v in pairs:
    if v>=0.9: buckets["j>=0.9"]+=1
    elif v>=0.5: buckets["j>=0.5"]+=1
    elif v>=0.2: buckets["j>=0.2"]+=1
    else: buckets["j<0.2"]+=1
par={l:l for l in L}
def find(x):
    while par[x]!=x: par[x]=par[par[x]]; x=par[x]
    return x
for a,b,v in pairs:
    if v>=0.5:
        ra,rb=find(a),find(b)
        if ra!=rb: par[ra]=rb
cl=collections.defaultdict(list)
for l in L: cl[find(l)].append(l)
clusters=sorted(cl.values(),key=len,reverse=True)
res={"layer_pairs":len(pairs),"jaccard_buckets":dict(buckets),
     "n_layers":len(L),"clusters":[{"layers":len(c),"files":sum(len(layers[l]) for l in c)} for c in clusters if len(c)>1]}
# shared bytes: same-cluster vs cross-cluster duplicate file bytes
owner={}; firstb={}
dup_same=0; dup_cross=0
for l in L:
    g=find(l)
    for m,(sz,) in [(m,(layers[l][m],)) for m in layers[l]]:
        if m in owner:
            if owner[m]==g: dup_same+=layers[l][m]
            else: dup_cross+=layers[l][m]
        else:
            owner[m]=g
res["whole_file_dup_same_cluster_bytes"]=dup_same
res["whole_file_dup_cross_cluster_bytes"]=dup_cross
# E4: dictionary choice per cluster
def zc(path,dp=None):
    cmd=["zstd","-19","-q","-c"]
    if dp: cmd+=["-D",dp]
    cmd.append(path)
    r=subprocess.run(cmd,capture_output=True)
    return len(r.stdout) if r.returncode==0 else None
def ztrain(paths,out):
    r=subprocess.run(["zstd","--train","-o",out,"-q"]+paths,capture_output=True,text=True)
    return os.path.getsize(out) if r.returncode==0 else 0
random.seed(3)
top=[c for c in clusters if len(c)>=3][:3]
res["e4_cluster_dicts"]=[]
for ci,c in enumerate(top):
    fl=[]
    for line in open(CSV):
        q=line.rstrip("\n").split("\t")
        if len(q)<3: continue
        parts=q[0].split("/")
        ly=parts[parts.index("layers")+1]
        if find(ly)==find(c[0]):
            fl.append((q[0],int(q[1])))
    cand=[p for p,s in fl if 2048<=s<=500000 and os.path.isfile(p)]
    random.shuffle(cand)
    tr=cand[:150]; ev=[p for p in cand[150:230]][:60]
    sys.stderr.write("c%d fl %d cand %d tr %d ev %d\n"%(ci,len(fl),len(cand),len(tr),len(ev)))
    if len(ev)<10: continue
    dp="%s/dict-c%d"%(TMP,ci); ds=ztrain(tr,dp)
    base=wd=0; n=0
    for p in ev:
        b=zc(p)
        if b is None: continue
        n+=1; base+=b
        x=zc(p,dp) if ds else None
        if x is not None: wd+=x
    net=base-wd-ds
    res["e4_cluster_dicts"].append({"cluster_layers":len(c),"train":len(tr),"eval":n,
        "dict_size":ds,"base19":base,"with_dict":wd,"net_saved":net,
        "net_per_eval_file":round(net/max(n,1))})
json.dump(res,open(OUT,"w"),indent=1)
print(json.dumps(res,indent=1))
