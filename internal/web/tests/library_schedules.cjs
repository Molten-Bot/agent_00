// Scheduling lives in Workflow; the Library shortcut carries task options into a draft.
const {before,after,test}=require('node:test');
const assert=require('node:assert/strict');
const {chromium}=require('playwright');
const baseURL=process.env.HARNESS_TEST_URL||'http://127.0.0.1:7777';
let browser;
before(async()=>{browser=await chromium.launch({headless:true,args:['--no-sandbox']})});
after(async()=>{await browser?.close()});
test('Library scheduling creates a workflow draft with its prompt and repository options',async t=>{
 const page=await browser.newPage({viewport:{width:1280,height:900}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const task={name:'unit-test-coverage',displayName:'Unit Test Coverage',type:'testing',prompt:'Improve unit tests.',description:'Add useful coverage'};
 let doc={version:2,revision:0,workflows:[]};let legacyCalls=0;
 let releaseSettings;
 const settingsReady=new Promise(resolve=>{releaseSettings=resolve});
 let releaseDefinitions;
 const definitionsReady=new Promise(resolve=>{releaseDefinitions=resolve});
 t.after(async()=>{
   releaseSettings();
   releaseDefinitions();
   await page.close();
   assert.deepEqual(errors,[]);
 });
 await page.addInitScript(()=>{window.EventSource=class{}});
 await page.route('**/*',async route=>{const url=new URL(route.request().url());if(url.origin!==new URL(baseURL).origin)return route.abort();
 if(url.pathname==='/api/workflows'){
   if(route.request().method()==='PUT'){
     doc={...route.request().postDataJSON(),revision:doc.revision+1};
   } else {
     await definitionsReady;
   }
   return route.fulfill({json:doc});
 }
 if(url.pathname==='/api/workflows/schedules')return route.fulfill({json:[]});
 if(url.pathname==='/api/library')return route.fulfill({json:{ok:true,tasks:[task]}});
 if(url.pathname==='/api/library/schedules'){legacyCalls++;return route.fulfill({status:410,json:{error:'Scheduling moved to Workflow'}})}
 if(url.pathname==='/api/github/repos')return route.fulfill({json:{ok:true,repos:[{full_name:'acme/repo',html_url:'https://github.com/acme/repo'}]}});
 if(url.pathname==='/api/agent-auth')return route.fulfill({json:{ok:true,auth:{required:false,ready:true,state:'ready'}}});
 if(url.pathname==='/api/review-settings'){
   await settingsReady;
   return route.fulfill({json:{ok:true,settings:{workflow_enabled:true,default_library_enabled:true}}});
 }
 if(url.pathname==='/api/status')return route.fulfill({json:{tasks:[],events:[],repositories:[],stats:{},resources:{}}});
 if(url.pathname.startsWith('/api/'))return route.fulfill({json:{ok:true}});return route.continue()});
 await page.goto(baseURL+'/#studio-library');
 const repoURL='https://github.com/acme/repo';
 const repositories=page.locator('#library-repo-select');
 await repositories.locator('option', {hasText:'acme/repo'}).waitFor({state:'attached'});
 assert.equal(await page.locator('#library-repo-input').inputValue(),'','repository stays empty until explicitly selected');
 await repositories.selectOption(repoURL);
 await page.locator('#library-base-branch').fill('feature');
 await page.locator('#library-target-subdir').fill('src');
 const schedule=page.getByRole('button',{name:'Create scheduled workflow for Unit Test Coverage'});
 const repository=page.getByRole('button',{name:'acme/repo',exact:true});
 try {
   await schedule.click();
   releaseSettings();
   // Let the sole repository become available before the workflow document.
   await repository.waitFor();
   assert.equal(await repository.getAttribute('aria-pressed'),'false','the pending draft owns repository selection');
   releaseDefinitions();
   await page.getByLabel('Trigger type').waitFor();
   assert.equal(new URL(page.url()).hash,'#workflow');
   assert.equal(await page.getByLabel('Trigger type').inputValue(),'schedule');
   assert.equal(await repository.getAttribute('aria-pressed'),'true');
 } catch (error) {
   t.diagnostic(JSON.stringify(await page.evaluate(() => ({
     hash: location.hash,
     display: state.appDisplay,
     settingsLoaded: state.settingsLoaded,
     workflowEnabled: state.reviewSettings.workflowEnabled,
     settingsOpen: state.settingsOpen,
     selectedTask: state.selectedLibraryTask,
     hasRepo: Boolean(libraryRepoInput.value),
     hasPendingSchedule: Boolean(window.pendingWorkflowSchedule),
     editorLoaded: typeof window.mountWorkflowEditor === 'function',
     workflowCards: document.querySelectorAll('.wf-step').length,
     inspectorHeading: document.querySelector('.wf-inspector h2')?.textContent,
     promptStatus: localPromptStatus.textContent,
   }))));
   t.diagnostic(JSON.stringify({savedWorkflows:doc.workflows.length,pageErrors:errors}));
   throw error;
 }
 assert.equal(await page.locator('#library-schedule-modal').count(),0);assert.equal(await page.locator('#library-schedules-open').count(),0);
 await page.getByRole('button',{name:'weeks',exact:true}).click();assert.equal(await page.getByLabel('Repeat every (weeks)').inputValue(),'1');await page.getByRole('button',{name:'hours',exact:true}).click();await page.getByLabel('Repeat every (hours)').fill('10');
 await page.locator('[data-id="task"]').click();assert.equal(await page.getByLabel('Prompt source',{exact:true}).inputValue(),'unit-test-coverage');assert.equal(await page.getByLabel('Base branch (optional)').inputValue(),'feature');assert.equal(await page.getByLabel('Target directory (optional)').inputValue(),'src');
 await page.getByLabel('Additional context (optional)').fill('Focus on API coverage');await page.locator('.wf-save-state:empty').waitFor({state:'attached'});
 assert.equal(doc.workflows.length,1);assert.equal(doc.workflows[0].enabled,false);assert.equal(doc.workflows[0].repository,'acme/repo');assert.equal(doc.workflows[0].task.baseBranch,'feature');assert.equal(doc.workflows[0].task.targetSubdir,'src');assert.equal(doc.workflows[0].trigger.intervalHours,10);assert.equal(doc.workflows[0].task.libraryTaskName,'unit-test-coverage');assert.equal(doc.workflows[0].task.instructions,'Focus on API coverage');assert.equal(legacyCalls,0);
 await page.reload();await page.locator('.wf-step').first().click();assert.equal(await page.getByLabel('Repeat every (hours)').inputValue(),'10');
});
