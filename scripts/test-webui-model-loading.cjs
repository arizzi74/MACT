#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage(); page.setDefaultTimeout(10000);
  const errors=[];page.on('pageerror',error=>errors.push(error.message));
  const session={ session_id:'session',codex_thread_id:'thread',worker_id:'worker',runtime_id:'runtime',name:'Model loading',state:'idle',stats:{model:'codex-model',reasoning_effort:'low'} };
  await page.route('https://models.test/**',route=>{
    const url=new URL(route.request().url());
    const json=body=>route.fulfill({contentType:'application/json',body:JSON.stringify(body)});
    if(url.pathname.endsWith('/admin/session'))return json({authenticated:true,owner_id:'owner',session_id:'login',server_time:new Date().toISOString(),expires_at:new Date(Date.now()+28800000).toISOString()});
    if(url.pathname.endsWith('/login/options'))return json({passkey:true,password:false});
    if(url.pathname.endsWith('/webui/sessions'))return json({sessions:[session],workers:[],runtimes:[]});
    if(url.pathname.endsWith('/telegram-bots'))return json({bots:[],revision:'1'});
    if(url.pathname.endsWith('/push/config'))return json({supported:false,subscribed:false});
    const name=url.pathname.includes('/static/')?path.basename(url.pathname):'webui.html';
    if(!fs.existsSync(path.join(assets,name)))return route.fulfill({status:404,body:''});
    return route.fulfill({contentType:name.endsWith('.js')?'text/javascript':name.endsWith('.css')?'text/css':'text/html',body:fs.readFileSync(path.join(assets,name))});
  });
  await page.addInitScript(session=>{
    window.modelCalls=[];window.modelSocket=null;
    window.WebSocket=class {
      static OPEN=1;
      constructor(url){this.activity=String(url).includes('/activity');this.readyState=0;if(!this.activity)window.modelSocket=this;
        setTimeout(()=>{this.readyState=1;this.emit(this.activity?{type:'activity_snapshot',version:3,sequence:0,revision:1,sessions:[{session_id:session.session_id,state:'idle',active_turn_id:'',pending_questions:0}],telegram_bots:[],telegram_bots_revision:'1'}:{type:'ready'});},1);
      }
      emit(value){this.onmessage?.({data:JSON.stringify(value)});}
      close(){this.readyState=3;this.onclose?.({code:1000});}
      releaseModels(){this.emit({id:this.heldModel.id,result:{data:[{id:'codex-model',model:'codex-model',displayName:'Coding model',supportedReasoningEfforts:[{reasoningEffort:'low'},{reasoningEffort:'high'}]}]}});}
      send(raw){const frame=JSON.parse(raw);if(!frame.method)return;window.modelCalls.push(frame);let result={};
        if(frame.method==='thread/resume')result={thread:{id:'thread'},model:'codex-model',reasoningEffort:'low'};
        else if(frame.method==='thread/turns/list')result={data:[],nextCursor:null};
        else if(frame.method==='thread/items/list')result={data:[],nextCursor:null};
        else if(frame.method==='gateway/questions')result={questions:[]};
        else if(frame.method==='thread/goal/get')result={goal:null};
        else if(frame.method==='account/rateLimits/read')result={rateLimits:null};
        else if(frame.method==='model/list'){this.heldModel=frame;return;}
        else if(frame.method==='gateway/command')result={text:'Setting applied.'};
        setTimeout(()=>this.emit({id:frame.id,result}),1);
      }
    };
  },session);
  try {
    await page.goto('https://models.test/tgw/webui/');
    await page.locator('[data-session-id="session"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    await page.waitForFunction(()=>window.modelSocket.heldModel);
    await page.locator('#prompt').fill('/reasoning');await page.locator('#send').click();
    await page.locator('#command-content').getByRole('button',{name:'Choose model and reasoning',exact:true}).waitFor();
    assert.equal(await page.locator('#command-content [aria-current="true"]').count(),0);
    await page.evaluate(()=>window.modelSocket.releaseModels());
    const current=page.locator('#command-content .command-choice[aria-current="true"]');
    await current.waitFor({timeout:2000});
    assert.match(await current.textContent(),/low/i,'The open reasoning menu updates when the delayed catalog arrives');
    assert.equal(await page.evaluate(()=>window.modelCalls.filter(frame=>frame.method==='gateway/command'||frame.method==='turn/start').length),0,'Catalog arrival never submits a setting or prompt');
    await current.click();
    await page.waitForFunction(()=>document.querySelector('#command-panel').hidden);
    const calls=await page.evaluate(()=>window.modelCalls.filter(frame=>frame.method==='gateway/command'));
    assert.equal(calls.length,1);assert.deepEqual(calls[0].params,{name:'reasoning',args:'low'});
    assert.deepEqual(errors,[]);
    console.log('Delayed model catalog refreshes the open reasoning menu without sending a command.');
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1;});
