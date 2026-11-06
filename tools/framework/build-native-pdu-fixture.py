#!/usr/bin/env python3
"""Link an offline serial/packet fixture against an already built candidate Host."""
import argparse,pathlib,shlex,subprocess
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--build',type=pathlib.Path,required=True);p.add_argument('--output',type=pathlib.Path,required=True);a=p.parse_args();build=a.build.resolve();out=a.output.resolve();out.parent.mkdir(parents=True,exist_ok=True);flags={}
for line in (build/'CMakeFiles/dvmtests.dir/flags.make').read_text().splitlines():
 if ' = ' in line:
  k,v=line.split(' = ',1);flags[k]=v
obj=out.with_suffix('.o');source=(pathlib.Path(__file__).resolve().parent.parent/'native_fixture/native-pdu-fixture.cpp');command=['/usr/bin/g++']+shlex.split(flags['CXX_DEFINES'])+shlex.split(flags['CXX_INCLUDES'])+shlex.split(flags['CXX_FLAGS'])+['-c',str(source),'-o',str(obj)];subprocess.run(command,check=True)
link=shlex.split((build/'CMakeFiles/dvmtests.dir/link.txt').read_text());link=[x for x in link if not x.startswith('CMakeFiles/dvmtests.dir/tests/') and 'libCatch2' not in x];link[link.index('-o')+1]=str(out);link.insert(link.index('-o'),str(obj));subprocess.run(link,cwd=build,check=True)
