function ok(body:any){return {status:200,body};}
function search(query:string,count:number,offset:number=0){
 const found=ether.catalog.search(query,60,0).titles.filter((x:any)=>x.type==='movie');
 const chosen=[];for(let i=0;i<Math.min(count,found.length);i++){const x=found[(offset+i)%found.length];const detail=ether.catalog.get(String(x.id));chosen.push({id:String(x.id),title:detail.title||x.title,year:x.year,summary:detail.summary||'No synopsis available.',genres:(detail.genres||[]).join(', ')});}return {query,total:found.length,movies:chosen};
}
function count(v:any){return Math.max(1,Math.min(5,Math.floor(Number(v)||3)));}
export function handle(req:any){
 if(req.path==='/ether/configure')return ok({errors:{}});
 if(req.path==='/ether/health')return ok({status:'ok'});
 if(req.path==='/ether/tool/pick_movie_night')return ok(search(String(req.body?.query||'star').slice(0,120),count(req.body?.count)));
 if(req.path==='/ether/action/pick'){
  const settings=ether.settings.get(),query=String(settings.query||'star'),n=count(settings.count),offset=Number(ether.kv.get('offset')||0);
  const result=search(query,n,offset);ether.kv.put('shortlist',JSON.stringify(result));ether.kv.put('offset',String(offset+n));return ok({toast:result.movies.length?'Saved '+result.movies.length+' movies for tonight.':'No movie matches. Try another title search in Setup.',refresh:true});
 }
 if(req.path==='/ether/view/tonight'){
 const saved=JSON.parse(ether.kv.get('shortlist')||'{"movies":[]}');
 return ok({title:'Movie Night',spec:{specVersion:1,layout:'detail',header:[{kind:'page-header',title:{$:'title'},description:{$:'subtitle'}}],body:[{kind:'panel',title:'$t:controls',blocks:[{kind:'text',content:'$t:empty'},{kind:'widget',widget:'ether.actionButton',props:{label:'$t:pick',action:'pick'}}]},{kind:'panel',title:'$t:shortlist',blocks:[{kind:'repeat',items:{$:'movies'},itemKey:{$:'id'},blocks:[{kind:'stat-tile',iconKey:'film',accent:'primary',label:{$:'label'},value:{$:'year'}},{kind:'text',content:{$:'summary'}}]}]}]},data:{title:'Movie Night',subtitle:saved.query?'Saved matches for “'+saved.query+'”. Pick again to rotate through '+saved.total+' movie matches.':'A shortlist you can keep while deciding what to watch.',movies:saved.movies.map((m:any)=>({...m,label:m.title+(m.genres?' · '+m.genres:''),year:String(m.year||'')}))}});
 }
 return {status:404,body:{error:{code:'not_found',message:'Route not found.'}}};
}
(globalThis as any).handle=handle;
